package main

import (
	"devzat/pkg/server"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"

	"devzat/pkg"
)

const (
	errCodeAllGood = iota
	errCodeBroadcastTimeout
)

const (
	msgGoingDown = `
	DevzatServer going down!

	This is probably because it is being updated. Try joining back immediately.

	If you still can't join, try joining back in 2 minutes. If you _still_ can't join, make an issue at github.com/quackduck/devzat/issues
	`
)

const (
	fmtErrInit   = "could not init chat server: %v"
	fmtErrParse  = "could not parse server options: %v"
	fmtProfiling = "Starting chat server on port %d and profiling on port %d\n"
	fmtRecover   = "The server almost crashed, send this to the devs: %v, stack: %v"
)

const (
	defaultSshPubKeyFile = "/.ssh/id_rsa"
	defaultPort          = 34450
	defaultScrollback    = 16
	defaultProfilePort   = 5555
)

const (
	appName     = "devzat-server"
	cfgFileName = "config.json"
)

// DevzatServer is a wrapper with extra methods that we only use here
type DevzatServer struct {
	server.Server
}

func (srv *DevzatServer) Init() error {
	rand.Seed(time.Now().Unix())

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("could not find config dir: %v", err)
	}

	// init the underlying server impl
	if errInit := srv.Server.Init(); errInit != nil {
		return fmt.Errorf(fmtErrInit, errInit)
	}

	srv.SetConfigDir(filepath.Join(cfgDir, appName))
	srv.SetConfigFileName(cfgFileName)

	if errConfig := srv.SaveConfigFile(); errConfig != nil {
		return fmt.Errorf("could not save config file: %v", errConfig)
	}

	srv.Port = defaultPort
	srv.Scrollback = defaultScrollback
	srv.ProfilePort = defaultProfilePort

	// parse any cli flags
	if errParse := srv.parseOptions(); errParse != nil {
		return fmt.Errorf(fmtErrParse, errParse)
	}

	fmt.Printf(fmtProfiling, srv.Port, srv.ProfilePort)

	// our threads
	go srv.dwellHttpServe() // TODO: have a web dashboard that shows logs
	go srv.sshRun()
	//	go srv.GetMsgsFromSlack()
	srv.dwellGracefulShutdown()

	return nil
}

func (srv *DevzatServer) dwellGracefulShutdown() {
	errChan := make(chan os.Signal, 2)
	signal.Notify(errChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	<-errChan
	fmt.Println("Shutting down...")

	_ = srv.SaveBans()

	time.AfterFunc(time.Second, func() {
		srv.Info().Msg("Broadcast taking too long, exiting server early.")
		os.Exit(errCodeBroadcastTimeout)
	})

	botName := srv.MainRoom().Bot().Name()

	srv.UniverseBroadcast(botName, msgGoingDown)

	os.Exit(errCodeAllGood)
}

func (srv *DevzatServer) sshRun() {
	pubKey := os.Getenv("HOME") + defaultSshPubKeyFile
	options := ssh.HostKeyFile(pubKey)
	strPort := fmt.Sprintf(":%d", srv.Port)

	ssh.Handle(srv.makeUserConnectionFunc())

	go srv.sshServeOn443(options)

	if err := ssh.ListenAndServe(strPort, nil, options, ssh.PublicKeyAuth(allowAllKeys)); err != nil {
		fmt.Println(err)
	}
}

func (srv *DevzatServer) sshServeOn443(options ssh.Option) {
	if srv.Port != 22 {
		return
	}

	fmt.Println("Also starting chat server on port 443")

	if err := ssh.ListenAndServe(":443", nil, options); err != nil {
		fmt.Println(err)
	}
}

func (srv *DevzatServer) parseOptions() (err error) {
	envPort := os.Getenv(pkg.EnvServerPort)
	if envPort != "" {
		if srv.Port, err = strconv.Atoi(envPort); err != nil {
			return fmt.Errorf("could not parse server port option: %v", err)
		}
	}

	// Check for global offline for backwards compatibility
	if os.Getenv(pkg.EnvOffline) != "" {
		srv.Slack.Offline = true
		srv.Twitter.Offline = true
	}

	return nil
}

func allowAllKeys(_ ssh.Context, _ ssh.PublicKey) bool {
	return true // allow all keys, this lets us hash pubkeys later
}

func (srv *DevzatServer) makeUserConnectionFunc() func(ssh.Session) {
	return func(s ssh.Session) {
		u, err := srv.NewUserFromSSH(s)
		if err != nil {
			srv.Info().Msgf("could not create user: %v", err)

			return
		}

		if u == nil {
			srv.Info().Msg("user is nil, closing session")
			_ = s.Close()

			return
		}

		defer func() { // crash protection
			if i := recover(); i != nil {
				srv.Warn().Msgf("error recovered: %v, %v", i, debug.Stack())
				srv.MainRoom().BotCast(fmt.Sprintf(fmtRecover, i, debug.Stack()))
			}
		}()

		u.Repl()
	}
}

func (srv *DevzatServer) dwellHttpServe() {
	if err := http.ListenAndServe(fmt.Sprintf(":%d", srv.ProfilePort), nil); err != nil {
		fmt.Println(err)
	}
}
