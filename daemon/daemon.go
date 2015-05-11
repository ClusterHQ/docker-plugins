package daemon

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/docker/libcontainer/label"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api"
	"github.com/docker/docker/autogen/dockerversion"
	"github.com/docker/docker/daemon/events"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/execdriver/execdrivers"
	"github.com/docker/docker/daemon/execdriver/lxc"
	"github.com/docker/docker/daemon/graphdriver"
	_ "github.com/docker/docker/daemon/graphdriver/vfs"
	"github.com/docker/docker/daemon/network"
	"github.com/docker/docker/daemon/networkdriver/bridge"
	"github.com/docker/docker/graph"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/broadcastwriter"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/docker/docker/pkg/graphdb"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/docker/docker/pkg/resolvconf"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/pkg/truncindex"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/trust"
	"github.com/docker/docker/utils"
	"github.com/docker/docker/volume"
	"github.com/docker/docker/volume/local"

	"github.com/go-fsnotify/fsnotify"
)

const defaultVolumesPathName = "volumes"

var (
	validContainerNameChars   = `[a-zA-Z0-9][a-zA-Z0-9_.-]`
	validContainerNamePattern = regexp.MustCompile(`^/?` + validContainerNameChars + `+$`)
)

type contStore struct {
	s map[string]*Container
	sync.Mutex
}

func (c *contStore) Add(id string, cont *Container) {
	c.Lock()
	c.s[id] = cont
	c.Unlock()
}

func (c *contStore) Get(id string) *Container {
	c.Lock()
	res := c.s[id]
	c.Unlock()
	return res
}

func (c *contStore) Delete(id string) {
	c.Lock()
	delete(c.s, id)
	c.Unlock()
}

func (c *contStore) List() []*Container {
	containers := new(History)
	c.Lock()
	for _, cont := range c.s {
		containers.Add(cont)
	}
	c.Unlock()
	containers.Sort()
	return *containers
}

type Daemon struct {
	ID               string
	repository       string
	sysInitPath      string
	containers       *contStore
	execCommands     *execStore
	graph            *graph.Graph
	repositories     *graph.TagStore
	idIndex          *truncindex.TruncIndex
	sysInfo          *sysinfo.SysInfo
	config           *Config
	containerGraph   *graphdb.Database
	driver           graphdriver.Driver
	execDriver       execdriver.Driver
	statsCollector   *statsCollector
	defaultLogConfig runconfig.LogConfig
	RegistryService  *registry.Service
	EventsService    *events.Events
	root             string
}

// Get looks for a container using the provided information, which could be
// one of the following inputs from the caller:
//  - A full container ID, which will exact match a container in daemon's list
//  - A container name, which will only exact match via the GetByName() function
//  - A partial container ID prefix (e.g. short ID) of any length that is
//    unique enough to only return a single container object
//  If none of these searches succeed, an error is returned
func (daemon *Daemon) Get(prefixOrName string) (*Container, error) {
	if containerByID := daemon.containers.Get(prefixOrName); containerByID != nil {
		// prefix is an exact match to a full container ID
		return containerByID, nil
	}

	// GetByName will match only an exact name provided; we ignore errors
	containerByName, _ := daemon.GetByName(prefixOrName)
	containerId, indexError := daemon.idIndex.Get(prefixOrName)

	if containerByName != nil {
		// prefix is an exact match to a full container Name
		return containerByName, nil
	}

	if containerId != "" {
		// prefix is a fuzzy match to a container ID
		return daemon.containers.Get(containerId), nil
	}
	return nil, indexError
}

// Exists returns a true if a container of the specified ID or name exists,
// false otherwise.
func (daemon *Daemon) Exists(id string) bool {
	c, _ := daemon.Get(id)
	return c != nil
}

func (daemon *Daemon) containerRoot(id string) string {
	return path.Join(daemon.repository, id)
}

// Load reads the contents of a container from disk
// This is typically done at startup.
func (daemon *Daemon) load(id string) (*Container, error) {
	container := &Container{
		root:         daemon.containerRoot(id),
		State:        NewState(),
		execCommands: newExecStore(),
	}
	if err := container.FromDisk(); err != nil {
		return nil, err
	}

	if container.ID != id {
		return container, fmt.Errorf("Container %s is stored at %s", container.ID, id)
	}

	return container, nil
}

// Register makes a container object usable by the daemon as <container.ID>
// This is a wrapper for register
func (daemon *Daemon) Register(container *Container) error {
	return daemon.register(container, true)
}

// register makes a container object usable by the daemon as <container.ID>
func (daemon *Daemon) register(container *Container, updateSuffixarray bool) error {
	if container.daemon != nil || daemon.Exists(container.ID) {
		return fmt.Errorf("Container is already loaded")
	}
	if err := validateID(container.ID); err != nil {
		return err
	}
	if err := daemon.ensureName(container); err != nil {
		return err
	}

	container.daemon = daemon

	// Attach to stdout and stderr
	container.stderr = broadcastwriter.New()
	container.stdout = broadcastwriter.New()
	// Attach to stdin
	if container.Config.OpenStdin {
		container.stdin, container.stdinPipe = io.Pipe()
	} else {
		container.stdinPipe = ioutils.NopWriteCloser(ioutil.Discard) // Silently drop stdin
	}
	// done
	daemon.containers.Add(container.ID, container)

	// don't update the Suffixarray if we're starting up
	// we'll waste time if we update it for every container
	daemon.idIndex.Add(container.ID)

	if err := daemon.verifyOldVolumesInfo(container); err != nil {
		return err
	}

	for _, config := range container.MountPoints {
		if len(config.Driver) > 0 {
			v, err := daemon.createVolume(config.Name, config.Driver)
			if err != nil {
				return err
			}
			config.Volume = v
		}
	}

	// FIXME: if the container is supposed to be running but is not, auto restart it?
	//        if so, then we need to restart monitor and init a new lock
	// If the container is supposed to be running, make sure of it
	if container.IsRunning() {
		logrus.Debugf("killing old running container %s", container.ID)

		container.SetStopped(&execdriver.ExitStatus{ExitCode: 0})

		// We only have to handle this for lxc because the other drivers will ensure that
		// no processes are left when docker dies
		if container.ExecDriver == "" || strings.Contains(container.ExecDriver, "lxc") {
			lxc.KillLxc(container.ID, 9)
		} else {
			// use the current driver and ensure that the container is dead x.x
			cmd := &execdriver.Command{
				ID: container.ID,
			}
			daemon.execDriver.Terminate(cmd)
		}

		if err := container.Unmount(); err != nil {
			logrus.Debugf("unmount error %s", err)
		}
		if err := container.ToDisk(); err != nil {
			logrus.Debugf("saving stopped state to disk %s", err)
		}
	}

	return nil
}

func (daemon *Daemon) ensureName(container *Container) error {
	if container.Name == "" {
		name, err := daemon.generateNewName(container.ID)
		if err != nil {
			return err
		}
		container.Name = name

		if err := container.ToDisk(); err != nil {
			logrus.Debugf("Error saving container name %s", err)
		}
	}
	return nil
}

func (daemon *Daemon) restore() error {
	var (
		debug         = (os.Getenv("DEBUG") != "" || os.Getenv("TEST") != "")
		containers    = make(map[string]*Container)
		currentDriver = daemon.driver.String()
	)

	if !debug {
		logrus.Info("Loading containers: start.")
	}
	dir, err := ioutil.ReadDir(daemon.repository)
	if err != nil {
		return err
	}

	for _, v := range dir {
		id := v.Name()
		container, err := daemon.load(id)
		if !debug && logrus.GetLevel() == logrus.InfoLevel {
			fmt.Print(".")
		}
		if err != nil {
			logrus.Errorf("Failed to load container %v: %v", id, err)
			continue
		}

		// Ignore the container if it does not support the current driver being used by the graph
		if (container.Driver == "" && currentDriver == "aufs") || container.Driver == currentDriver {
			logrus.Debugf("Loaded container %v", container.ID)

			containers[container.ID] = container
		} else {
			logrus.Debugf("Cannot load container %s because it was created with another graph driver.", container.ID)
		}
	}

	registeredContainers := []*Container{}

	if entities := daemon.containerGraph.List("/", -1); entities != nil {
		for _, p := range entities.Paths() {
			if !debug && logrus.GetLevel() == logrus.InfoLevel {
				fmt.Print(".")
			}

			e := entities[p]

			if container, ok := containers[e.ID()]; ok {
				if err := daemon.register(container, false); err != nil {
					logrus.Debugf("Failed to register container %s: %s", container.ID, err)
				}

				registeredContainers = append(registeredContainers, container)

				// delete from the map so that a new name is not automatically generated
				delete(containers, e.ID())
			}
		}
	}

	// Any containers that are left over do not exist in the graph
	for _, container := range containers {
		// Try to set the default name for a container if it exists prior to links
		container.Name, err = daemon.generateNewName(container.ID)
		if err != nil {
			logrus.Debugf("Setting default id - %s", err)
		}

		if err := daemon.register(container, false); err != nil {
			logrus.Debugf("Failed to register container %s: %s", container.ID, err)
		}

		registeredContainers = append(registeredContainers, container)
	}

	// check the restart policy on the containers and restart any container with
	// the restart policy of "always"
	if daemon.config.AutoRestart {
		logrus.Debug("Restarting containers...")

		for _, container := range registeredContainers {
			if container.hostConfig.RestartPolicy.Name == "always" ||
				(container.hostConfig.RestartPolicy.Name == "on-failure" && container.ExitCode != 0) {
				logrus.Debugf("Starting container %s", container.ID)

				if err := container.Start(); err != nil {
					logrus.Debugf("Failed to start container %s: %s", container.ID, err)
				}
			}
		}
	}

	if !debug {
		if logrus.GetLevel() == logrus.InfoLevel {
			fmt.Println()
		}
		logrus.Info("Loading containers: done.")
	}

	return nil
}

// set up the watch on the host's /etc/resolv.conf so that we can update container's
// live resolv.conf when the network changes on the host
func (daemon *Daemon) setupResolvconfWatcher() error {

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	//this goroutine listens for the events on the watch we add
	//on the resolv.conf file on the host
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Name == "/etc/resolv.conf" &&
					(event.Op&(fsnotify.Write|fsnotify.Create) != 0) {
					// verify a real change happened before we go further--a file write may have happened
					// without an actual change to the file
					updatedResolvConf, newResolvConfHash, err := resolvconf.GetIfChanged()
					if err != nil {
						logrus.Debugf("Error retrieving updated host resolv.conf: %v", err)
					} else if updatedResolvConf != nil {
						// because the new host resolv.conf might have localhost nameservers..
						updatedResolvConf, modified := resolvconf.FilterResolvDns(updatedResolvConf, daemon.config.Bridge.EnableIPv6)
						if modified {
							// changes have occurred during localhost cleanup: generate an updated hash
							newHash, err := ioutils.HashData(bytes.NewReader(updatedResolvConf))
							if err != nil {
								logrus.Debugf("Error generating hash of new resolv.conf: %v", err)
							} else {
								newResolvConfHash = newHash
							}
						}
						logrus.Debug("host network resolv.conf changed--walking container list for updates")
						contList := daemon.containers.List()
						for _, container := range contList {
							if err := container.updateResolvConf(updatedResolvConf, newResolvConfHash); err != nil {
								logrus.Debugf("Error on resolv.conf update check for container ID: %s: %v", container.ID, err)
							}
						}
					}
				}
			case err := <-watcher.Errors:
				logrus.Debugf("host resolv.conf notify error: %v", err)
			}
		}
	}()

	if err := watcher.Add("/etc"); err != nil {
		return err
	}
	return nil
}

func (daemon *Daemon) checkDeprecatedExpose(config *runconfig.Config) bool {
	if config != nil {
		if config.PortSpecs != nil {
			for _, p := range config.PortSpecs {
				if strings.Contains(p, ":") {
					return true
				}
			}
		}
	}
	return false
}

func (daemon *Daemon) mergeAndVerifyConfig(config *runconfig.Config, img *image.Image) ([]string, error) {
	warnings := []string{}
	if (img != nil && daemon.checkDeprecatedExpose(img.Config)) || daemon.checkDeprecatedExpose(config) {
		warnings = append(warnings, "The mapping to public ports on your host via Dockerfile EXPOSE (host:port:port) has been deprecated. Use -p to publish the ports.")
	}
	if img != nil && img.Config != nil {
		if err := runconfig.Merge(config, img.Config); err != nil {
			return nil, err
		}
	}
	if config.Entrypoint.Len() == 0 && config.Cmd.Len() == 0 {
		return nil, fmt.Errorf("No command specified")
	}
	return warnings, nil
}

func (daemon *Daemon) generateIdAndName(name string) (string, string, error) {
	var (
		err error
		id  = stringid.GenerateRandomID()
	)

	if name == "" {
		if name, err = daemon.generateNewName(id); err != nil {
			return "", "", err
		}
		return id, name, nil
	}

	if name, err = daemon.reserveName(id, name); err != nil {
		return "", "", err
	}

	return id, name, nil
}

func (daemon *Daemon) reserveName(id, name string) (string, error) {
	if !validContainerNamePattern.MatchString(name) {
		return "", fmt.Errorf("Invalid container name (%s), only %s are allowed", name, validContainerNameChars)
	}

	if name[0] != '/' {
		name = "/" + name
	}

	if _, err := daemon.containerGraph.Set(name, id); err != nil {
		if !graphdb.IsNonUniqueNameError(err) {
			return "", err
		}

		conflictingContainer, err := daemon.GetByName(name)
		if err != nil {
			if strings.Contains(err.Error(), "Could not find entity") {
				return "", err
			}

			// Remove name and continue starting the container
			if err := daemon.containerGraph.Delete(name); err != nil {
				return "", err
			}
		} else {
			nameAsKnownByUser := strings.TrimPrefix(name, "/")
			return "", fmt.Errorf(
				"Conflict. The name %q is already in use by container %s. You have to delete (or rename) that container to be able to reuse that name.", nameAsKnownByUser,
				stringid.TruncateID(conflictingContainer.ID))
		}
	}
	return name, nil
}

func (daemon *Daemon) generateNewName(id string) (string, error) {
	var name string
	for i := 0; i < 6; i++ {
		name = namesgenerator.GetRandomName(i)
		if name[0] != '/' {
			name = "/" + name
		}

		if _, err := daemon.containerGraph.Set(name, id); err != nil {
			if !graphdb.IsNonUniqueNameError(err) {
				return "", err
			}
			continue
		}
		return name, nil
	}

	name = "/" + stringid.TruncateID(id)
	if _, err := daemon.containerGraph.Set(name, id); err != nil {
		return "", err
	}
	return name, nil
}

func (daemon *Daemon) generateHostname(id string, config *runconfig.Config) {
	// Generate default hostname
	// FIXME: the lxc template no longer needs to set a default hostname
	if config.Hostname == "" {
		config.Hostname = id[:12]
	}
}

func (daemon *Daemon) getEntrypointAndArgs(configEntrypoint *runconfig.Entrypoint, configCmd *runconfig.Command) (string, []string) {
	var (
		entrypoint string
		args       []string
	)

	cmdSlice := configCmd.Slice()
	if configEntrypoint.Len() != 0 {
		eSlice := configEntrypoint.Slice()
		entrypoint = eSlice[0]
		args = append(eSlice[1:], cmdSlice...)
	} else {
		entrypoint = cmdSlice[0]
		args = cmdSlice[1:]
	}
	return entrypoint, args
}

func parseSecurityOpt(container *Container, config *runconfig.HostConfig) error {
	var (
		labelOpts []string
		err       error
	)

	for _, opt := range config.SecurityOpt {
		con := strings.SplitN(opt, ":", 2)
		if len(con) == 1 {
			return fmt.Errorf("Invalid --security-opt: %q", opt)
		}
		switch con[0] {
		case "label":
			labelOpts = append(labelOpts, con[1])
		case "apparmor":
			container.AppArmorProfile = con[1]
		default:
			return fmt.Errorf("Invalid --security-opt: %q", opt)
		}
	}

	container.ProcessLabel, container.MountLabel, err = label.InitLabels(labelOpts)
	return err
}

func (daemon *Daemon) newContainer(name string, config *runconfig.Config, imgID string) (*Container, error) {
	var (
		id  string
		err error
	)
	id, name, err = daemon.generateIdAndName(name)
	if err != nil {
		return nil, err
	}

	daemon.generateHostname(id, config)
	entrypoint, args := daemon.getEntrypointAndArgs(config.Entrypoint, config.Cmd)

	container := &Container{
		ID:              id,
		Created:         time.Now().UTC(),
		Path:            entrypoint,
		Args:            args, //FIXME: de-duplicate from config
		Config:          config,
		hostConfig:      &runconfig.HostConfig{},
		ImageID:         imgID,
		NetworkSettings: &network.Settings{},
		Name:            name,
		Driver:          daemon.driver.String(),
		ExecDriver:      daemon.execDriver.Name(),
		State:           NewState(),
		execCommands:    newExecStore(),
		MountPoints:     map[string]*MountPoint{},
	}
	container.root = daemon.containerRoot(container.ID)
	return container, err
}

func (daemon *Daemon) createRootfs(container *Container) error {
	// Step 1: create the container directory.
	// This doubles as a barrier to avoid race conditions.
	if err := os.Mkdir(container.root, 0700); err != nil {
		return err
	}
	initID := fmt.Sprintf("%s-init", container.ID)
	if err := daemon.driver.Create(initID, container.ImageID); err != nil {
		return err
	}
	initPath, err := daemon.driver.Get(initID, "")
	if err != nil {
		return err
	}
	defer daemon.driver.Put(initID)

	if err := graph.SetupInitLayer(initPath); err != nil {
		return err
	}

	if err := daemon.driver.Create(container.ID, initID); err != nil {
		return err
	}
	return nil
}

func GetFullContainerName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("Container name cannot be empty")
	}
	if name[0] != '/' {
		name = "/" + name
	}
	return name, nil
}

func (daemon *Daemon) GetByName(name string) (*Container, error) {
	fullName, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	entity := daemon.containerGraph.Get(fullName)
	if entity == nil {
		return nil, fmt.Errorf("Could not find entity for %s", name)
	}
	e := daemon.containers.Get(entity.ID())
	if e == nil {
		return nil, fmt.Errorf("Could not find container for entity id %s", entity.ID())
	}
	return e, nil
}

func (daemon *Daemon) Children(name string) (map[string]*Container, error) {
	name, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	children := make(map[string]*Container)

	err = daemon.containerGraph.Walk(name, func(p string, e *graphdb.Entity) error {
		c, err := daemon.Get(e.ID())
		if err != nil {
			return err
		}
		children[p] = c
		return nil
	}, 0)

	if err != nil {
		return nil, err
	}
	return children, nil
}

func (daemon *Daemon) Parents(name string) ([]string, error) {
	name, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}

	return daemon.containerGraph.Parents(name)
}

func (daemon *Daemon) RegisterLink(parent, child *Container, alias string) error {
	fullName := path.Join(parent.Name, alias)
	if !daemon.containerGraph.Exists(fullName) {
		_, err := daemon.containerGraph.Set(fullName, child.ID)
		return err
	}
	return nil
}

func (daemon *Daemon) RegisterLinks(container *Container, hostConfig *runconfig.HostConfig) error {
	if hostConfig != nil && hostConfig.Links != nil {
		for _, l := range hostConfig.Links {
			parts, err := parsers.PartParser("name:alias", l)
			if err != nil {
				return err
			}
			child, err := daemon.Get(parts["name"])
			if err != nil {
				//An error from daemon.Get() means this name could not be found
				return fmt.Errorf("Could not get container for %s", parts["name"])
			}
			for child.hostConfig.NetworkMode.IsContainer() {
				parts := strings.SplitN(string(child.hostConfig.NetworkMode), ":", 2)
				child, err = daemon.Get(parts[1])
				if err != nil {
					return fmt.Errorf("Could not get container for %s", parts[1])
				}
			}
			if child.hostConfig.NetworkMode.IsHost() {
				return runconfig.ErrConflictHostNetworkAndLinks
			}
			if err := daemon.RegisterLink(container, child, parts["alias"]); err != nil {
				return err
			}
		}

		// After we load all the links into the daemon
		// set them to nil on the hostconfig
		hostConfig.Links = nil
		if err := container.WriteHostConfig(); err != nil {
			return err
		}
	}
	return nil
}

func NewDaemon(config *Config, registryService *registry.Service) (daemon *Daemon, err error) {
	if config.Mtu == 0 {
		config.Mtu = getDefaultNetworkMtu()
	}
	// Check for mutually incompatible config options
	if config.Bridge.Iface != "" && config.Bridge.IP != "" {
		return nil, fmt.Errorf("You specified -b & --bip, mutually exclusive options. Please specify only one.")
	}
	if !config.Bridge.EnableIptables && !config.Bridge.InterContainerCommunication {
		return nil, fmt.Errorf("You specified --iptables=false with --icc=false. ICC uses iptables to function. Please set --icc or --iptables to true.")
	}
	if !config.Bridge.EnableIptables && config.Bridge.EnableIpMasq {
		config.Bridge.EnableIpMasq = false
	}
	config.DisableNetwork = config.Bridge.Iface == disableNetworkBridge

	// Check that the system is supported and we have sufficient privileges
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("The Docker daemon is only supported on linux")
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("The Docker daemon needs to be run as root")
	}
	if err := checkKernel(); err != nil {
		return nil, err
	}

	// set up the tmpDir to use a canonical path
	tmp, err := tempDir(config.Root)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the TempDir under %s: %s", config.Root, err)
	}
	realTmp, err := fileutils.ReadSymlinkedDirectory(tmp)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the full path to the TempDir (%s): %s", tmp, err)
	}
	os.Setenv("TMPDIR", realTmp)

	// get the canonical path to the Docker root directory
	var realRoot string
	if _, err := os.Stat(config.Root); err != nil && os.IsNotExist(err) {
		realRoot = config.Root
	} else {
		realRoot, err = fileutils.ReadSymlinkedDirectory(config.Root)
		if err != nil {
			return nil, fmt.Errorf("Unable to get the full path to root (%s): %s", config.Root, err)
		}
	}
	config.Root = realRoot
	// Create the root directory if it doesn't exists
	if err := os.MkdirAll(config.Root, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	// Set the default driver
	graphdriver.DefaultDriver = config.GraphDriver

	// Load storage driver
	driver, err := graphdriver.New(config.Root, config.GraphOptions)
	if err != nil {
		return nil, fmt.Errorf("error initializing graphdriver: %v", err)
	}
	logrus.Debugf("Using graph driver %s", driver)

	d := &Daemon{}
	d.driver = driver

	defer func() {
		if err != nil {
			if err := d.Shutdown(); err != nil {
				logrus.Error(err)
			}
		}
	}()

	if config.EnableSelinuxSupport {
		if selinuxEnabled() {
			// As Docker on btrfs and SELinux are incompatible at present, error on both being enabled
			if d.driver.String() == "btrfs" {
				return nil, fmt.Errorf("SELinux is not supported with the BTRFS graph driver")
			}
			logrus.Debug("SELinux enabled successfully")
		} else {
			logrus.Warn("Docker could not enable SELinux on the host system")
		}
	} else {
		selinuxSetDisabled()
	}

	daemonRepo := path.Join(config.Root, "containers")

	if err := os.MkdirAll(daemonRepo, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	// Migrate the container if it is aufs and aufs is enabled
	if err := migrateIfAufs(d.driver, config.Root); err != nil {
		return nil, err
	}

	logrus.Debug("Creating images graph")
	g, err := graph.NewGraph(path.Join(config.Root, "graph"), d.driver)
	if err != nil {
		return nil, err
	}

	volumesDriver, err := local.New(filepath.Join(config.Root, defaultVolumesPathName))
	if err != nil {
		return nil, err
	}
	volume.Drivers.Register(volumesDriver, volumesDriver.Name())

	trustKey, err := api.LoadOrCreateTrustKey(config.TrustKeyPath)
	if err != nil {
		return nil, err
	}

	trustDir := path.Join(config.Root, "trust")
	if err := os.MkdirAll(trustDir, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	trustService, err := trust.NewTrustStore(trustDir)
	if err != nil {
		return nil, fmt.Errorf("could not create trust store: %s", err)
	}

	eventsService := events.New()
	logrus.Debug("Creating repository list")
	tagCfg := &graph.TagStoreConfig{
		Graph:    g,
		Key:      trustKey,
		Registry: registryService,
		Events:   eventsService,
		Trust:    trustService,
	}
	repositories, err := graph.NewTagStore(path.Join(config.Root, "repositories-"+d.driver.String()), tagCfg)
	if err != nil {
		return nil, fmt.Errorf("Couldn't create Tag store: %s", err)
	}

	if !config.DisableNetwork {
		if err := bridge.InitDriver(&config.Bridge); err != nil {
			return nil, fmt.Errorf("Error initializing Bridge: %v", err)
		}
	}

	graphdbPath := path.Join(config.Root, "linkgraph.db")
	graph, err := graphdb.NewSqliteConn(graphdbPath)
	if err != nil {
		return nil, err
	}

	d.containerGraph = graph

	localCopy := path.Join(config.Root, "init", fmt.Sprintf("dockerinit-%s", dockerversion.VERSION))
	sysInitPath := utils.DockerInitPath(localCopy)
	if sysInitPath == "" {
		return nil, fmt.Errorf("Could not locate dockerinit: This usually means docker was built incorrectly. See https://docs.docker.com/contributing/devenvironment for official build instructions.")
	}

	if sysInitPath != localCopy {
		// When we find a suitable dockerinit binary (even if it's our local binary), we copy it into config.Root at localCopy for future use (so that the original can go away without that being a problem, for example during a package upgrade).
		if err := os.Mkdir(path.Dir(localCopy), 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if _, err := fileutils.CopyFile(sysInitPath, localCopy); err != nil {
			return nil, err
		}
		if err := os.Chmod(localCopy, 0700); err != nil {
			return nil, err
		}
		sysInitPath = localCopy
	}

	sysInfo := sysinfo.New(false)
	const runDir = "/var/run/docker"
	ed, err := execdrivers.NewDriver(config.ExecDriver, config.ExecOptions, runDir, config.Root, sysInitPath, sysInfo)
	if err != nil {
		return nil, err
	}

	d.ID = trustKey.PublicKey().KeyID()
	d.repository = daemonRepo
	d.containers = &contStore{s: make(map[string]*Container)}
	d.execCommands = newExecStore()
	d.graph = g
	d.repositories = repositories
	d.idIndex = truncindex.NewTruncIndex([]string{})
	d.sysInfo = sysInfo
	d.config = config
	d.sysInitPath = sysInitPath
	d.execDriver = ed
	d.statsCollector = newStatsCollector(1 * time.Second)
	d.defaultLogConfig = config.LogConfig
	d.RegistryService = registryService
	d.EventsService = eventsService
	d.root = config.Root

	if err := d.restore(); err != nil {
		return nil, err
	}

	// set up filesystem watch on resolv.conf for network changes
	if err := d.setupResolvconfWatcher(); err != nil {
		return nil, err
	}

	return d, nil
}

func (d *Daemon) createVolume(name, driverName string) (volume.Volume, error) {
	vd, err := d.getVolumeDriver(driverName)
	if err != nil {
		return nil, err
	}
	return vd.Create(name)
}

func (d *Daemon) removeVolume(v volume.Volume) error {
	vd, err := d.getVolumeDriver(v.DriverName())
	if err != nil {
		return nil
	}
	return vd.Remove(v)
}

func (d *Daemon) getVolumeDriver(name string) (volume.Driver, error) {
	if name == "" {
		name = "local"
	}
	vd := volume.Drivers.Lookup(name)
	if vd == nil {
		return nil, fmt.Errorf("Volumes Driver %s isn't registered", name)
	}
	return vd, nil
}

func (daemon *Daemon) Shutdown() error {
	if daemon.containerGraph != nil {
		if err := daemon.containerGraph.Close(); err != nil {
			logrus.Errorf("Error during container graph.Close(): %v", err)
		}
	}
	if daemon.driver != nil {
		if err := daemon.driver.Cleanup(); err != nil {
			logrus.Errorf("Error during graph storage driver.Cleanup(): %v", err)
		}
	}
	if daemon.containers != nil {
		group := sync.WaitGroup{}
		logrus.Debug("starting clean shutdown of all containers...")
		for _, container := range daemon.List() {
			c := container
			if c.IsRunning() {
				logrus.Debugf("stopping %s", c.ID)
				group.Add(1)

				go func() {
					defer group.Done()
					if err := c.KillSig(15); err != nil {
						logrus.Debugf("kill 15 error for %s - %s", c.ID, err)
					}
					c.WaitStop(-1 * time.Second)
					logrus.Debugf("container stopped %s", c.ID)
				}()
			}
		}
		group.Wait()
	}

	return nil
}

func (daemon *Daemon) Mount(container *Container) error {
	dir, err := daemon.driver.Get(container.ID, container.GetMountLabel())
	if err != nil {
		return fmt.Errorf("Error getting container %s from driver %s: %s", container.ID, daemon.driver, err)
	}
	if container.basefs == "" {
		container.basefs = dir
	} else if container.basefs != dir {
		daemon.driver.Put(container.ID)
		return fmt.Errorf("Error: driver %s is returning inconsistent paths for container %s ('%s' then '%s')",
			daemon.driver, container.ID, container.basefs, dir)
	}
	return nil
}

func (daemon *Daemon) Unmount(container *Container) error {
	daemon.driver.Put(container.ID)
	return nil
}

func (daemon *Daemon) Changes(container *Container) ([]archive.Change, error) {
	initID := fmt.Sprintf("%s-init", container.ID)
	return daemon.driver.Changes(container.ID, initID)
}

func (daemon *Daemon) Diff(container *Container) (archive.Archive, error) {
	initID := fmt.Sprintf("%s-init", container.ID)
	return daemon.driver.Diff(container.ID, initID)
}

func (daemon *Daemon) Run(c *Container, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (execdriver.ExitStatus, error) {
	return daemon.execDriver.Run(c.command, pipes, startCallback)
}

func (daemon *Daemon) Pause(c *Container) error {
	if err := daemon.execDriver.Pause(c.command); err != nil {
		return err
	}
	c.SetPaused()
	return nil
}

func (daemon *Daemon) Unpause(c *Container) error {
	if err := daemon.execDriver.Unpause(c.command); err != nil {
		return err
	}
	c.SetUnpaused()
	return nil
}

func (daemon *Daemon) Kill(c *Container, sig int) error {
	return daemon.execDriver.Kill(c.command, sig)
}

func (daemon *Daemon) Stats(c *Container) (*execdriver.ResourceStats, error) {
	return daemon.execDriver.Stats(c.ID)
}

func (daemon *Daemon) SubscribeToContainerStats(name string) (chan interface{}, error) {
	c, err := daemon.Get(name)
	if err != nil {
		return nil, err
	}
	ch := daemon.statsCollector.collect(c)
	return ch, nil
}

func (daemon *Daemon) UnsubscribeToContainerStats(name string, ch chan interface{}) error {
	c, err := daemon.Get(name)
	if err != nil {
		return err
	}
	daemon.statsCollector.unsubscribe(c, ch)
	return nil
}

// FIXME: this is a convenience function for integration tests
// which need direct access to daemon.graph.
// Once the tests switch to using engine and jobs, this method
// can go away.
func (daemon *Daemon) Graph() *graph.Graph {
	return daemon.graph
}

func (daemon *Daemon) Repositories() *graph.TagStore {
	return daemon.repositories
}

func (daemon *Daemon) Config() *Config {
	return daemon.config
}

func (daemon *Daemon) SystemConfig() *sysinfo.SysInfo {
	return daemon.sysInfo
}

func (daemon *Daemon) SystemInitPath() string {
	return daemon.sysInitPath
}

func (daemon *Daemon) GraphDriver() graphdriver.Driver {
	return daemon.driver
}

func (daemon *Daemon) ExecutionDriver() execdriver.Driver {
	return daemon.execDriver
}

func (daemon *Daemon) ContainerGraph() *graphdb.Database {
	return daemon.containerGraph
}

func (daemon *Daemon) ImageGetCached(imgID string, config *runconfig.Config) (*image.Image, error) {
	// Retrieve all images
	images, err := daemon.Graph().Map()
	if err != nil {
		return nil, err
	}

	// Store the tree in a map of map (map[parentId][childId])
	imageMap := make(map[string]map[string]struct{})
	for _, img := range images {
		if _, exists := imageMap[img.Parent]; !exists {
			imageMap[img.Parent] = make(map[string]struct{})
		}
		imageMap[img.Parent][img.ID] = struct{}{}
	}

	// Loop on the children of the given image and check the config
	var match *image.Image
	for elem := range imageMap[imgID] {
		img, ok := images[elem]
		if !ok {
			return nil, fmt.Errorf("unable to find image %q", elem)
		}
		if runconfig.Compare(&img.ContainerConfig, config) {
			if match == nil || match.Created.Before(img.Created) {
				match = img
			}
		}
	}
	return match, nil
}

// tempDir returns the default directory to use for temporary files.
func tempDir(rootDir string) (string, error) {
	var tmpDir string
	if tmpDir = os.Getenv("DOCKER_TMPDIR"); tmpDir == "" {
		tmpDir = filepath.Join(rootDir, "tmp")
	}
	return tmpDir, os.MkdirAll(tmpDir, 0700)
}

func checkKernel() error {
	// Check for unsupported kernel versions
	// FIXME: it would be cleaner to not test for specific versions, but rather
	// test for specific functionalities.
	// Unfortunately we can't test for the feature "does not cause a kernel panic"
	// without actually causing a kernel panic, so we need this workaround until
	// the circumstances of pre-3.8 crashes are clearer.
	// For details see https://github.com/docker/docker/issues/407
	if k, err := kernel.GetKernelVersion(); err != nil {
		logrus.Warnf("%s", err)
	} else {
		if kernel.CompareKernelVersion(k, &kernel.KernelVersionInfo{Kernel: 3, Major: 8, Minor: 0}) < 0 {
			if os.Getenv("DOCKER_NOWARN_KERNEL_VERSION") == "" {
				logrus.Warnf("You are running linux kernel version %s, which might be unstable running docker. Please upgrade your kernel to 3.8.0.", k.String())
			}
		}
	}
	return nil
}

func (daemon *Daemon) verifyHostConfig(hostConfig *runconfig.HostConfig) ([]string, error) {
	var warnings []string

	if hostConfig == nil {
		return warnings, nil
	}

	if hostConfig.LxcConf.Len() > 0 && !strings.Contains(daemon.ExecutionDriver().Name(), "lxc") {
		return warnings, fmt.Errorf("Cannot use --lxc-conf with execdriver: %s", daemon.ExecutionDriver().Name())
	}
	if hostConfig.Memory != 0 && hostConfig.Memory < 4194304 {
		return warnings, fmt.Errorf("Minimum memory limit allowed is 4MB")
	}
	if hostConfig.Memory > 0 && !daemon.SystemConfig().MemoryLimit {
		warnings = append(warnings, "Your kernel does not support memory limit capabilities. Limitation discarded.")
		hostConfig.Memory = 0
	}
	if hostConfig.Memory > 0 && hostConfig.MemorySwap != -1 && !daemon.SystemConfig().SwapLimit {
		warnings = append(warnings, "Your kernel does not support swap limit capabilities, memory limited without swap.")
		hostConfig.MemorySwap = -1
	}
	if hostConfig.Memory > 0 && hostConfig.MemorySwap > 0 && hostConfig.MemorySwap < hostConfig.Memory {
		return warnings, fmt.Errorf("Minimum memoryswap limit should be larger than memory limit, see usage.")
	}
	if hostConfig.Memory == 0 && hostConfig.MemorySwap > 0 {
		return warnings, fmt.Errorf("You should always set the Memory limit when using Memoryswap limit, see usage.")
	}
	if hostConfig.CpuQuota > 0 && !daemon.SystemConfig().CpuCfsQuota {
		warnings = append(warnings, "Your kernel does not support CPU cfs quota. Quota discarded.")
		hostConfig.CpuQuota = 0
	}

	return warnings, nil
}

func (daemon *Daemon) setHostConfig(container *Container, hostConfig *runconfig.HostConfig) error {
	container.Lock()
	defer container.Unlock()
	if err := parseSecurityOpt(container, hostConfig); err != nil {
		return err
	}

	// Register any links from the host config before starting the container
	if err := daemon.RegisterLinks(container, hostConfig); err != nil {
		return err
	}

	if err := daemon.registerMountPoints(container, hostConfig); err != nil {
		return err
	}

	container.hostConfig = hostConfig
	container.toDisk()
	return nil
}
