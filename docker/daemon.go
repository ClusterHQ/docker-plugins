// +build daemon

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Sirupsen/logrus"
	apiserver "github.com/docker/docker/api/server"
	"github.com/docker/docker/autogen/dockerversion"
	"github.com/docker/docker/daemon"
	_ "github.com/docker/docker/daemon/execdriver/lxc"
	_ "github.com/docker/docker/daemon/execdriver/native"
	"github.com/docker/docker/pkg/homedir"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/pkg/pidfile"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/pkg/timeutils"
	"github.com/docker/docker/plugins"
	"github.com/docker/docker/registry"
)

const CanDaemon = true

var (
	daemonCfg   = &daemon.Config{}
	registryCfg = &registry.Options{}
)

func init() {
	if daemonCfg.LogConfig.Config == nil {
		daemonCfg.LogConfig.Config = make(map[string]string)
	}
	daemonCfg.InstallFlags()
	registryCfg.InstallFlags()
}

func migrateKey() (err error) {
	// Migrate trust key if exists at ~/.docker/key.json and owned by current user
	oldPath := filepath.Join(homedir.Get(), ".docker", defaultTrustKeyFile)
	newPath := filepath.Join(getDaemonConfDir(), defaultTrustKeyFile)
	if _, statErr := os.Stat(newPath); os.IsNotExist(statErr) && currentUserIsOwner(oldPath) {
		defer func() {
			// Ensure old path is removed if no error occurred
			if err == nil {
				err = os.Remove(oldPath)
			} else {
				logrus.Warnf("Key migration failed, key file not removed at %s", oldPath)
			}
		}()

		if err := os.MkdirAll(getDaemonConfDir(), os.FileMode(0644)); err != nil {
			return fmt.Errorf("Unable to create daemon configuration directory: %s", err)
		}

		newFile, err := os.OpenFile(newPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return fmt.Errorf("error creating key file %q: %s", newPath, err)
		}
		defer newFile.Close()

		oldFile, err := os.Open(oldPath)
		if err != nil {
			return fmt.Errorf("error opening key file %q: %s", oldPath, err)
		}
		defer oldFile.Close()

		if _, err := io.Copy(newFile, oldFile); err != nil {
			return fmt.Errorf("error copying key: %s", err)
		}

		logrus.Infof("Migrated key from %s to %s", oldPath, newPath)
	}

	return nil
}

func mainDaemon() {
	if flag.NArg() != 0 {
		flag.Usage()
		return
	}

	logrus.SetFormatter(&logrus.TextFormatter{TimestampFormat: timeutils.RFC3339NanoFixed})

	var pfile *pidfile.PidFile
	if daemonCfg.Pidfile != "" {
		pf, err := pidfile.New(daemonCfg.Pidfile)
		if err != nil {
			logrus.Fatalf("Error starting daemon: %v", err)
		}
		pfile = pf
		defer func() {
			if err := pfile.Remove(); err != nil {
				logrus.Error(err)
			}
		}()
	}

	if err := migrateKey(); err != nil {
		logrus.Fatal(err)
	}
	daemonCfg.TrustKeyPath = *flTrustKey

	registryService := registry.NewService(registryCfg)
	d, err := daemon.NewDaemon(daemonCfg, registryService)
	if err != nil {
		if pfile != nil {
			if err := pfile.Remove(); err != nil {
				logrus.Error(err)
			}
		}
		logrus.Fatalf("Error starting daemon: %v", err)
	}

	logrus.Info("Daemon has completed initialization")

	logrus.WithFields(logrus.Fields{
		"version":     dockerversion.VERSION,
		"commit":      dockerversion.GITCOMMIT,
		"execdriver":  d.ExecutionDriver().Name(),
		"graphdriver": d.GraphDriver().String(),
	}).Info("Docker daemon")

	serverConfig := &apiserver.ServerConfig{
		Logging:     true,
		EnableCors:  daemonCfg.EnableCors,
		CorsHeaders: daemonCfg.CorsHeaders,
		Version:     dockerversion.VERSION,
		SocketGroup: daemonCfg.SocketGroup,
		Tls:         *flTls,
		TlsVerify:   *flTlsVerify,
		TlsCa:       *flCa,
		TlsCert:     *flCert,
		TlsKey:      *flKey,
	}

	api := apiserver.New(serverConfig)

	// The serve API routine never exits unless an error occurs
	// We need to start it as a goroutine and wait on it so
	// daemon doesn't exit
	serveAPIWait := make(chan error)
	go func() {
		if err := api.ServeApi(flHosts); err != nil {
			logrus.Errorf("ServeAPI error: %v", err)
			serveAPIWait <- err
			return
		}
		serveAPIWait <- nil
	}()

	if err := migrateKey(); err != nil {
		logrus.Fatal(err)
	}
	daemonCfg.TrustKeyPath = *flTrustKey

	registryService := registry.NewService(registryCfg)
	d, err := daemon.NewDaemon(daemonCfg, registryService)
	if err != nil {
		if pfile != nil {
			if err := pfile.Remove(); err != nil {
				logrus.Error(err)
			}
		}
		logrus.Fatalf("Error starting daemon: %v", err)
	}

	logrus.Info("Daemon has completed initialization")

	logrus.WithFields(logrus.Fields{
		"version":     dockerversion.VERSION,
		"commit":      dockerversion.GITCOMMIT,
		"execdriver":  d.ExecutionDriver().Name(),
		"graphdriver": d.GraphDriver().String(),
	}).Info("Docker daemon")

	signal.Trap(func() {
		api.Close()
		<-serveAPIWait
		shutdownDaemon(d, 15)
		if pfile != nil {
			if err := pfile.Remove(); err != nil {
				logrus.Error(err)
			}
		}
	})

	// after the daemon is done setting up we can tell the api to start
	// accepting connections with specified daemon
	api.AcceptConnections(d)

	// now load plugins, which may assume the API and daemon are running
	if err := plugins.Load(); err != nil {
		logrus.Error(err)
	}

	// Daemon is fully initialized and handling API traffic
	// Wait for serve API to complete
	errAPI := <-serveAPIWait
	shutdownDaemon(d, 15)
	if errAPI != nil {
		if pfile != nil {
			if err := pfile.Remove(); err != nil {
				logrus.Error(err)
			}
		}
		logrus.Fatalf("Shutting down due to ServeAPI error: %v", errAPI)
	}
}

// shutdownDaemon just wraps daemon.Shutdown() to handle a timeout in case
// d.Shutdown() is waiting too long to kill container or worst it's
// blocked there
func shutdownDaemon(d *daemon.Daemon, timeout time.Duration) {
	ch := make(chan struct{})
	go func() {
		d.Shutdown()
		close(ch)
	}()
	select {
	case <-ch:
		logrus.Debug("Clean shutdown succeded")
	case <-time.After(timeout * time.Second):
		logrus.Error("Force shutdown daemon")
	}
}

// currentUserIsOwner checks whether the current user is the owner of the given
// file.
func currentUserIsOwner(f string) bool {
	if fileInfo, err := system.Stat(f); err == nil && fileInfo != nil {
		if int(fileInfo.Uid()) == os.Getuid() {
			return true
		}
	}
	return false
}
