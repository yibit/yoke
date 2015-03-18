package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/hoisie/mustache"
	_ "github.com/lib/pq"
)

// Piper is build to Pipe data from exec.Cmd objects to our logger
type Piper struct {
	Prefix string
	// just need a couple methods
}

// Write is just a fulfillment of the io.Writer interface
func (p Piper) Write(d []byte) (int, error) {
	log.Info("%s %s", p.Prefix, d)
	return len(d), nil
}

var cmd *exec.Cmd

var running bool

// Listen on the action channel and perform the action
func ActionStart() error {
	log.Info("[action.ActionStart]")

	running = false
	go func() {
		for {
			select {
			case act := <-actions:
				log.Info("[action] new action: " + act)
				doAction(act)
			}
		}
	}()

	return nil
}

// switch through the actions and perform the requested action
func doAction(act string) {
	switch act {
	case "kill":
		killDB()
	case "master":
		startMaster()
	case "slave":
		startSlave()
	case "single":
		startSingle()
	default:
		fmt.Println("i dont know what to do with this action: " + act)
	}
}

// Starts the database as a master node and sends its data file to the slave
func startMaster() {
	log.Debug("[action] start master")
	// make sure we have a database in the data folder
	initDB()
	log.Debug("[action] db init'ed")

	// set postgresql.conf as not master
	status.SetState("(master)configuring")
	configurePGConf(pgConfig{master: false, listenAddr: "0.0.0.0"})
	// set pg_hba.conf
	configureHBAConf()
	// delete recovery.conf
	destroyRecovery()
	// start the database
	status.SetState("(master)starting")
	startDB()

	// connect to DB and tell it to start backup
	db, err := sql.Open("postgres", fmt.Sprintf("user=%s database=postgres sslmode=disable host=localhost port=%d", SystemUser(), conf.PGPort))
	if err != nil {
		log.Fatal("[action.startMaster] Couldnt establish Database connection (%s)", err.Error())
		log.Close()
		os.Exit(1)
	}
	defer db.Close()

	_, err = db.Exec("select pg_start_backup('replication')")
	if err != nil {
		log.Error("[action.startMaster] Couldnt start backup (%s)", err.Error())
	}

	log.Debug("[action] backup started")
	// rsync my files over to the slave server
	status.SetState("(master)syncing")

	self := Whoami()
	other, _ := Whoisnot(self.CRole)

	for other == nil {
		log.Error("The other member shutdown while I was trying to sync my data. retrying...")
		other, _ = Whoisnot(self.CRole)
		time.Sleep(time.Second)
	}

	// rsync -a {{local_dir}} {{slave_ip}}:{{slave_dir}}
	sync := mustache.Render(conf.SyncCommand, map[string]string{"local_dir": conf.DataDir, "slave_ip": other.Ip, "slave_dir": other.DataDir})
	// cmd := strings.Split(sync, " ")
	sc := exec.Command("bash", "-c", sync) // cmd[0], cmd[1:]...)
	sc.Stdout = Piper{"[sync.stdout]"}
	sc.Stderr = Piper{"[sync.stderr]"}
	log.Debug("[action] running sync (%s)", sync)

	if err = sc.Run(); err != nil {
		log.Error("[action] sync failed.")
		log.Debug("[sync.error] message: %s", err.Error())
	}

	// connect to DB and tell it to stop backup
	_, err = db.Exec("select pg_stop_backup()")
	if err != nil {
		log.Fatal("[action.startMaster] Couldnt stop backup (%s)", err.Error())
		log.Close()
		os.Exit(1)
	}

	log.Debug("[action] backup complete")

	// set postgresql.conf as master
	configurePGConf(pgConfig{master: true, listenAddr: "0.0.0.0"})

	// start refresh/restart server
	log.Debug("[action] restarting DB")
	restartDB()

	// make sure slave is connected and in sync
	status.SetState("(master)waiting")
	defer status.SetState("(master)running")

	log.Debug("[action] db wait for sync")

	for {
		rows, err := db.Query("SELECT application_name, client_addr, state, sync_state, pg_xlog_location_diff(pg_current_xlog_location(), sent_location) FROM pg_stat_replication")
		if err != nil {

		}
		for rows.Next() {
			var name string
			var addr string
			var state string
			var sync string
			var behind int64
			err = rows.Scan(&name, &addr, &state, &sync, &behind)
			if err != nil {
				panic(err)
			}
			if behind > 0 {
				log.Info("[action] Sync is catching up (name:%s,address:%s,state:%s,sync:%s,behind:%d)", name, addr, state, sync, behind)
			} else {
				log.Debug("[action] db synced")
				addVip()
				roleChangeCommand("master")
				return
			}
		}
		time.Sleep(time.Second)
	}
}

// Starts the database as a slave node after waiting for master to come online
func startSlave() {
	// wait for master server to be running
	removeVip()
	roleChangeCommand("slave")
	status.SetState("(slave)waiting")
	log.Debug("[action] wait for master")
	self := Whoami()
	for {
		other, err := Whoisnot(self.CRole)
		if err != nil {
			log.Fatal("I have lost communication with the other server, I cannot start without it")
			status.SetState("(slave)master_lost")
			log.Close()
			os.Exit(1)
		}
		if other.State == "(master)running" || other.State == "(master)waiting" {
			break
		}
		time.Sleep(time.Second)
	}
	// set postgresql.conf as not master
	status.SetState("(slave)configuring")
	configurePGConf(pgConfig{master: false, listenAddr: "0.0.0.0"})
	// set pg_hba.conf
	configureHBAConf()
	// set recovery.conf
	createRecovery()
	// start the database
	status.SetState("(slave)starting")
	log.Debug("[action] starting database")
	startDB()

	// go into a waiting for sync signal from the master
	// this should allow us to detect if we can safely become master
	status.SetState("(slave)syncing")
	for {
		other, err := Whoisnot(self.CRole)
		if err != nil {
			log.Fatal("I have lost communication with the other server, I cannot start without it")
		}
		if other.State == "(master)running" {
			break
		}
		time.Sleep(time.Second)
	}	
	status.SetState("(slave)running")
}

// Starts the database as a single node
func startSingle() {
	log.Info("[action] starting DB as single")
	status.SetState("(single)configuring")
	// set postgresql.conf as not master
	configurePGConf(pgConfig{master: false, listenAddr: "0.0.0.0"})
	// set pg_hba.conf
	configureHBAConf()
	// delete recovery.conf
	destroyRecovery()
	// start the database
	status.SetState("(single)starting")
	startDB()
	status.SetState("(single)running")
	log.Info("[action] running DB as single")
	addVip()
	roleChangeCommand("single")
}

// this will kill the database that is running. reguardless of its current state
func killDB() {
	log.Debug("[action] KillingDB")

	// done in a defer because we might return early
	defer func() {
		status.SetState("(kill)down")
	}()
	// return if it was never created or up
	if cmd == nil {
		log.Debug("[action] nothing to kill")
		return
	}

	// db is no longer running
	if running == false {
		log.Debug("[action] already dead")
		cmd = nil
		return
	}
	// if it is running kill it and wait for it to go down
	status.SetState("(kill)signaling")
	err := cmd.Process.Signal(syscall.SIGQUIT)
	if err != nil {
		log.Error("[action] Kill Signal error: %s", err.Error())
	}

	// waiting for shutdown
	status.SetState("(kill)waiting")

	for running == true {
		log.Debug("[action] waiting to die")
		time.Sleep(time.Second)
	}
	cmd = nil
}

func startDB() {
	// if we still happen to have a cmd running kill it
	if cmd != nil {
		killDB()
	}
	log.Debug("[action] starting db")
	cmd = exec.Command("postgres", "-D", conf.DataDir)
	cmd.Stdout = Piper{"[postgres.stdout]"}
	cmd.Stderr = Piper{"[postgres.stderr]"}
	cmd.Start()
	log.Debug("[action] db started")
	running = true
	go waiter(cmd)
	time.Sleep(10 * time.Second)
	if running == false {
		log.Fatal("I just started the database and it is not running")
		log.Close()
		os.Exit(1)
	}
}

func restartDB() {
	killDB()
	startDB()
}

func initDB() {
	if _, err := os.Stat(conf.DataDir + "/postgresql.conf"); os.IsNotExist(err) {
		init := exec.Command("initdb", conf.DataDir)
		init.Stdout = Piper{"[initdb.stdout]"}
		init.Stderr = Piper{"[initdb.stderr]"}

		if err = init.Run(); err != nil {
			log.Fatal("[action] initdb failed. Are you missing your postgresql.conf")
			log.Close()
			os.Exit(1)
		}
	}
}

func waiter(c *exec.Cmd) {
	log.Debug("[action] Waiter waiting")
	err := c.Wait()
	if err != nil {
		log.Error("[action] Waiter Error: %s", err.Error())
	}

	// I should check to see if i exited and was not supposed to
	self := Whoami()
	if self.State == "(master)running" || self.State == "(slave)running" || self.State == "(single)running" {
		log.Fatal("the database exited and it wasnt supposed to")
		log.Close()
		os.Exit(1)
	}

	log.Debug("[action] Waiter done")
	running = false
}

func roleChangeCommand(role string) {
	if conf.RoleChangeCommand != "" {
		rcc := exec.Command("bash", "-c", fmt.Sprintf("%s %s", conf.RoleChangeCommand, role))
		rcc.Stdout = Piper{"[RoleChangeCommand.stdout]"}
		rcc.Stderr = Piper{"[RoleChangeCommand.stderr]"}
		if err := rcc.Run(); err != nil {
			log.Error("[action] RoleChangeCommand failed.")
			log.Debug("[RoleChangeCommand.error] message: %s", err.Error())
		}
	}
}

func addVip() {
	if vipable() {
		log.Info("[action] Adding VIP")
		vAddCmd := exec.Command("bash", "-c", fmt.Sprintf("%s %s", conf.VipAddCommand, conf.Vip))
		vAddCmd.Stdout = Piper{"[VIPAddCommand.stdout]"}
		vAddCmd.Stderr = Piper{"[VIPAddCommand.stderr]"}
		if err := vAddCmd.Run(); err != nil {
			log.Error("[action] VIPAddCommand failed.")
			log.Debug("[VIPAddCommand.error] message: %s", err.Error())
		}
	}
}

func removeVip() {
	if vipable() {
		log.Info("[action] Removing VIP")
		vRemoveCmd := exec.Command("bash", "-c", fmt.Sprintf("%s %s", conf.VipRemoveCommand, conf.Vip))
		vRemoveCmd.Stdout = Piper{"[VIPRemoveCommand.stdout]"}
		vRemoveCmd.Stderr = Piper{"[VIPRemoveCommand.stderr]"}
		if err := vRemoveCmd.Run(); err != nil {
			log.Error("[action] VIPRemoveCommand failed.")
			log.Debug("[VIPRemoveCommand.error] message: %s", err.Error())
		}
	}
}

func vipable() bool {
	return conf.Vip != "" && conf.VipAddCommand != "" && conf.VipRemoveCommand != ""
}

func SystemUser() string {
	username := "postgres"
	usr, err := user.Current()
	if err != nil {
		cmd := exec.Command("bash", "-c", "whoami")
		bytes, err := cmd.Output()
		if err == nil {
			str := string(bytes)
			return strings.TrimSpace(str)
		}
	}

	username = usr.Username
	return username
}
