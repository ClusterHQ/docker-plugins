package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/docker/libcontainer/configs"
	"github.com/docker/libcontainer/devices"
	"github.com/docker/libcontainer/label"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/journald"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/daemon/logger/syslog"
	"github.com/docker/docker/daemon/network"
	"github.com/docker/docker/daemon/networkdriver/bridge"
	"github.com/docker/docker/image"
	"github.com/docker/docker/links"
	"github.com/docker/docker/nat"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/broadcastwriter"
	"github.com/docker/docker/pkg/directory"
	"github.com/docker/docker/pkg/etchosts"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/resolvconf"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/symlink"
	"github.com/docker/docker/pkg/ulimit"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/utils"
	"github.com/docker/docker/volume"
)

const DefaultPathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

var (
	ErrNotATTY               = errors.New("The PTY is not a file")
	ErrNoTTY                 = errors.New("No PTY found")
	ErrContainerStart        = errors.New("The container failed to start. Unknown error")
	ErrContainerStartTimeout = errors.New("The container failed to start due to timed out.")
)

type StreamConfig struct {
	stdout    *broadcastwriter.BroadcastWriter
	stderr    *broadcastwriter.BroadcastWriter
	stdin     io.ReadCloser
	stdinPipe io.WriteCloser
}

type BindMount struct {
	Source      string
	Destination string
	RW          bool
}

type VolumeConfig struct {
	Driver      string
	Destination string
	RW          bool
}

type Container struct {
	StreamConfig
	*State `json:"State"` // Needed for remote api version <= 1.11
	root   string         // Path to the "home" of the container, including metadata.
	basefs string         // Path to the graphdriver mountpoint

	ID                       string
	Created                  time.Time
	Path                     string
	Args                     []string
	Config                   *runconfig.Config
	ImageID                  string `json:"Image"`
	NetworkSettings          *network.Settings
	ResolvConfPath           string
	HostnamePath             string
	HostsPath                string
	LogPath                  string
	Name                     string
	Driver                   string
	ExecDriver               string
	MountLabel, ProcessLabel string
	AppArmorProfile          string
	RestartCount             int
	UpdateDns                bool
	VolumeConfig             map[string]*VolumeConfig
	BindMounts               []*BindMount

	command      *execdriver.Command
	daemon       *Daemon
	hostConfig   *runconfig.HostConfig
	activeLinks  map[string]*links.Link
	monitor      *containerMonitor
	execCommands *execStore
	// logDriver for closing
	logDriver logger.Logger
	logCopier *logger.Copier
	// live volume objects
	volumes []volume.Volume
}

func (container *Container) FromDisk() error {
	pth, err := container.jsonPath()
	if err != nil {
		return err
	}

	jsonSource, err := os.Open(pth)
	if err != nil {
		return err
	}
	defer jsonSource.Close()

	dec := json.NewDecoder(jsonSource)

	// Load container settings
	// udp broke compat of docker.PortMapping, but it's not used when loading a container, we can skip it
	if err := dec.Decode(container); err != nil && !strings.Contains(err.Error(), "docker.PortMapping") {
		return err
	}

	if err := label.ReserveLabel(container.ProcessLabel); err != nil {
		return err
	}
	return container.readHostConfig()
}

func (container *Container) toDisk() error {
	data, err := json.Marshal(container)
	if err != nil {
		return err
	}

	pth, err := container.jsonPath()
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(pth, data, 0666); err != nil {
		return err
	}

	return container.WriteHostConfig()
}

func (container *Container) ToDisk() error {
	container.Lock()
	err := container.toDisk()
	container.Unlock()
	return err
}

func (container *Container) readHostConfig() error {
	container.hostConfig = &runconfig.HostConfig{}
	// If the hostconfig file does not exist, do not read it.
	// (We still have to initialize container.hostConfig,
	// but that's OK, since we just did that above.)
	pth, err := container.hostConfigPath()
	if err != nil {
		return err
	}

	_, err = os.Stat(pth)
	if os.IsNotExist(err) {
		return nil
	}

	f, err := os.Open(pth)
	if err != nil {
		return err
	}
	defer f.Close()

	return json.NewDecoder(f).Decode(&container.hostConfig)
}

func (container *Container) WriteHostConfig() error {
	data, err := json.Marshal(container.hostConfig)
	if err != nil {
		return err
	}

	pth, err := container.hostConfigPath()
	if err != nil {
		return err
	}

	return ioutil.WriteFile(pth, data, 0666)
}

func (container *Container) LogEvent(action string) {
	d := container.daemon
	d.EventsService.Log(
		action,
		container.ID,
		container.Config.Image,
	)
}

// Evaluates `path` in the scope of the container's basefs, with proper path
// sanitisation. Symlinks are all scoped to the basefs of the container, as
// though the container's basefs was `/`.
//
// The basefs of a container is the host-facing path which is bind-mounted as
// `/` inside the container. This method is essentially used to access a
// particular path inside the container as though you were a process in that
// container.
//
// NOTE: The returned path is *only* safely scoped inside the container's basefs
//       if no component of the returned path changes (such as a component
//       symlinking to a different path) between using this method and using the
//       path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetResourcePath(path string) (string, error) {
	cleanPath := filepath.Join("/", path)
	return symlink.FollowSymlinkInScope(filepath.Join(container.basefs, cleanPath), container.basefs)
}

// Evaluates `path` in the scope of the container's root, with proper path
// sanitisation. Symlinks are all scoped to the root of the container, as
// though the container's root was `/`.
//
// The root of a container is the host-facing configuration metadata directory.
// Only use this method to safely access the container's `container.json` or
// other metadata files. If in doubt, use container.GetResourcePath.
//
// NOTE: The returned path is *only* safely scoped inside the container's root
//       if no component of the returned path changes (such as a component
//       symlinking to a different path) between using this method and using the
//       path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetRootResourcePath(path string) (string, error) {
	cleanPath := filepath.Join("/", path)
	return symlink.FollowSymlinkInScope(filepath.Join(container.root, cleanPath), container.root)
}

func getDevicesFromPath(deviceMapping runconfig.DeviceMapping) (devs []*configs.Device, err error) {
	device, err := devices.DeviceFromPath(deviceMapping.PathOnHost, deviceMapping.CgroupPermissions)
	// if there was no error, return the device
	if err == nil {
		device.Path = deviceMapping.PathInContainer
		return append(devs, device), nil
	}

	// if the device is not a device node
	// try to see if it's a directory holding many devices
	if err == devices.ErrNotADevice {

		// check if it is a directory
		if src, e := os.Stat(deviceMapping.PathOnHost); e == nil && src.IsDir() {

			// mount the internal devices recursively
			filepath.Walk(deviceMapping.PathOnHost, func(dpath string, f os.FileInfo, e error) error {
				childDevice, e := devices.DeviceFromPath(dpath, deviceMapping.CgroupPermissions)
				if e != nil {
					// ignore the device
					return nil
				}

				// add the device to userSpecified devices
				childDevice.Path = strings.Replace(dpath, deviceMapping.PathOnHost, deviceMapping.PathInContainer, 1)
				devs = append(devs, childDevice)

				return nil
			})
		}
	}

	if len(devs) > 0 {
		return devs, nil
	}

	return devs, fmt.Errorf("error gathering device information while adding custom device %q: %s", deviceMapping.PathOnHost, err)
}

func populateCommand(c *Container, env []string) error {
	en := &execdriver.Network{
		Mtu:       c.daemon.config.Mtu,
		Interface: nil,
	}

	parts := strings.SplitN(string(c.hostConfig.NetworkMode), ":", 2)
	switch parts[0] {
	case "none":
	case "host":
		en.HostNetworking = true
	case "bridge", "": // empty string to support existing containers
		if !c.Config.NetworkDisabled {
			network := c.NetworkSettings
			en.Interface = &execdriver.NetworkInterface{
				Gateway:              network.Gateway,
				Bridge:               network.Bridge,
				IPAddress:            network.IPAddress,
				IPPrefixLen:          network.IPPrefixLen,
				MacAddress:           network.MacAddress,
				LinkLocalIPv6Address: network.LinkLocalIPv6Address,
				GlobalIPv6Address:    network.GlobalIPv6Address,
				GlobalIPv6PrefixLen:  network.GlobalIPv6PrefixLen,
				IPv6Gateway:          network.IPv6Gateway,
			}
		}
	case "container":
		nc, err := c.getNetworkedContainer()
		if err != nil {
			return err
		}
		en.ContainerID = nc.ID
	default:
		return fmt.Errorf("invalid network mode: %s", c.hostConfig.NetworkMode)
	}

	ipc := &execdriver.Ipc{}

	if c.hostConfig.IpcMode.IsContainer() {
		ic, err := c.getIpcContainer()
		if err != nil {
			return err
		}
		ipc.ContainerID = ic.ID
	} else {
		ipc.HostIpc = c.hostConfig.IpcMode.IsHost()
	}

	pid := &execdriver.Pid{}
	pid.HostPid = c.hostConfig.PidMode.IsHost()

	// Build lists of devices allowed and created within the container.
	var userSpecifiedDevices []*configs.Device
	for _, deviceMapping := range c.hostConfig.Devices {
		devs, err := getDevicesFromPath(deviceMapping)
		if err != nil {
			return err
		}

		userSpecifiedDevices = append(userSpecifiedDevices, devs...)
	}
	allowedDevices := append(configs.DefaultAllowedDevices, userSpecifiedDevices...)

	autoCreatedDevices := append(configs.DefaultAutoCreatedDevices, userSpecifiedDevices...)

	// TODO: this can be removed after lxc-conf is fully deprecated
	lxcConfig, err := mergeLxcConfIntoOptions(c.hostConfig)
	if err != nil {
		return err
	}

	var rlimits []*ulimit.Rlimit
	ulimits := c.hostConfig.Ulimits

	// Merge ulimits with daemon defaults
	ulIdx := make(map[string]*ulimit.Ulimit)
	for _, ul := range ulimits {
		ulIdx[ul.Name] = ul
	}
	for name, ul := range c.daemon.config.Ulimits {
		if _, exists := ulIdx[name]; !exists {
			ulimits = append(ulimits, ul)
		}
	}

	for _, limit := range ulimits {
		rl, err := limit.GetRlimit()
		if err != nil {
			return err
		}
		rlimits = append(rlimits, rl)
	}

	resources := &execdriver.Resources{
		Memory:         c.hostConfig.Memory,
		MemorySwap:     c.hostConfig.MemorySwap,
		CpuShares:      c.hostConfig.CpuShares,
		CpusetCpus:     c.hostConfig.CpusetCpus,
		CpusetMems:     c.hostConfig.CpusetMems,
		CpuQuota:       c.hostConfig.CpuQuota,
		Rlimits:        rlimits,
		OomKillDisable: c.hostConfig.OomKillDisable,
	}

	processConfig := execdriver.ProcessConfig{
		Privileged: c.hostConfig.Privileged,
		Entrypoint: c.Path,
		Arguments:  c.Args,
		Tty:        c.Config.Tty,
		User:       c.Config.User,
	}

	processConfig.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	processConfig.Env = env

	c.command = &execdriver.Command{
		ID:                 c.ID,
		Rootfs:             c.RootfsPath(),
		ReadonlyRootfs:     c.hostConfig.ReadonlyRootfs,
		InitPath:           "/.dockerinit",
		WorkingDir:         c.Config.WorkingDir,
		Network:            en,
		Ipc:                ipc,
		Pid:                pid,
		Resources:          resources,
		AllowedDevices:     allowedDevices,
		AutoCreatedDevices: autoCreatedDevices,
		CapAdd:             c.hostConfig.CapAdd,
		CapDrop:            c.hostConfig.CapDrop,
		ProcessConfig:      processConfig,
		ProcessLabel:       c.GetProcessLabel(),
		MountLabel:         c.GetMountLabel(),
		LxcConfig:          lxcConfig,
		AppArmorProfile:    c.AppArmorProfile,
		CgroupParent:       c.hostConfig.CgroupParent,
	}

	return nil
}

func (container *Container) Start() (err error) {
	container.Lock()
	defer container.Unlock()

	if container.Running {
		return nil
	}

	if container.removalInProgress || container.Dead {
		return fmt.Errorf("Container is marked for removal and cannot be started.")
	}

	// if we encounter an error during start we need to ensure that any other
	// setup has been cleaned up properly
	defer func() {
		if err != nil {
			container.setError(err)
			// if no one else has set it, make sure we don't leave it at zero
			if container.ExitCode == 0 {
				container.ExitCode = 128
			}
			container.toDisk()
			container.cleanup()
		}
	}()

	if err := container.setupContainerDns(); err != nil {
		return err
	}
	if err := container.Mount(); err != nil {
		return err
	}
	if err := container.initializeNetworking(); err != nil {
		return err
	}
	if err := container.updateParentsHosts(); err != nil {
		return err
	}
	container.verifyDaemonSettings()
	linkedEnv, err := container.setupLinkedContainers()
	if err != nil {
		return err
	}
	if err := container.setupWorkingDirectory(); err != nil {
		return err
	}
	env := container.createDaemonEnvironment(linkedEnv)
	if err := populateCommand(container, env); err != nil {
		return err
	}
	mounts, err := container.setupMounts()
	if err != nil {
		return err
	}
	container.command.Mounts = mounts
	return container.waitForStart()
}

func (container *Container) Run() error {
	if err := container.Start(); err != nil {
		return err
	}
	container.WaitStop(-1 * time.Second)
	return nil
}

func (container *Container) Output() (output []byte, err error) {
	pipe := container.StdoutPipe()
	defer pipe.Close()
	if err := container.Start(); err != nil {
		return nil, err
	}
	output, err = ioutil.ReadAll(pipe)
	container.WaitStop(-1 * time.Second)
	return output, err
}

// StreamConfig.StdinPipe returns a WriteCloser which can be used to feed data
// to the standard input of the container's active process.
// Container.StdoutPipe and Container.StderrPipe each return a ReadCloser
// which can be used to retrieve the standard output (and error) generated
// by the container's active process. The output (and error) are actually
// copied and delivered to all StdoutPipe and StderrPipe consumers, using
// a kind of "broadcaster".

func (streamConfig *StreamConfig) StdinPipe() io.WriteCloser {
	return streamConfig.stdinPipe
}

func (streamConfig *StreamConfig) StdoutPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stdout.AddWriter(writer, "")
	return ioutils.NewBufReader(reader)
}

func (streamConfig *StreamConfig) StderrPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stderr.AddWriter(writer, "")
	return ioutils.NewBufReader(reader)
}

func (streamConfig *StreamConfig) StdoutLogPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stdout.AddWriter(writer, "stdout")
	return ioutils.NewBufReader(reader)
}

func (streamConfig *StreamConfig) StderrLogPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stderr.AddWriter(writer, "stderr")
	return ioutils.NewBufReader(reader)
}

func (container *Container) buildHostnameFile() error {
	hostnamePath, err := container.GetRootResourcePath("hostname")
	if err != nil {
		return err
	}
	container.HostnamePath = hostnamePath

	if container.Config.Domainname != "" {
		return ioutil.WriteFile(container.HostnamePath, []byte(fmt.Sprintf("%s.%s\n", container.Config.Hostname, container.Config.Domainname)), 0644)
	}
	return ioutil.WriteFile(container.HostnamePath, []byte(container.Config.Hostname+"\n"), 0644)
}

func (container *Container) buildHostsFiles(IP string) error {

	hostsPath, err := container.GetRootResourcePath("hosts")
	if err != nil {
		return err
	}
	container.HostsPath = hostsPath

	var extraContent []etchosts.Record

	children, err := container.daemon.Children(container.Name)
	if err != nil {
		return err
	}

	for linkAlias, child := range children {
		_, alias := path.Split(linkAlias)
		// allow access to the linked container via the alias, real name, and container hostname
		aliasList := alias + " " + child.Config.Hostname
		// only add the name if alias isn't equal to the name
		if alias != child.Name[1:] {
			aliasList = aliasList + " " + child.Name[1:]
		}
		extraContent = append(extraContent, etchosts.Record{Hosts: aliasList, IP: child.NetworkSettings.IPAddress})
	}

	for _, extraHost := range container.hostConfig.ExtraHosts {
		// allow IPv6 addresses in extra hosts; only split on first ":"
		parts := strings.SplitN(extraHost, ":", 2)
		extraContent = append(extraContent, etchosts.Record{Hosts: parts[0], IP: parts[1]})
	}

	return etchosts.Build(container.HostsPath, IP, container.Config.Hostname, container.Config.Domainname, extraContent)
}

func (container *Container) buildHostnameAndHostsFiles(IP string) error {
	if err := container.buildHostnameFile(); err != nil {
		return err
	}

	return container.buildHostsFiles(IP)
}

func (container *Container) AllocateNetwork() error {
	mode := container.hostConfig.NetworkMode
	if container.Config.NetworkDisabled || !mode.IsPrivate() {
		return nil
	}

	var err error

	networkSettings, err := bridge.Allocate(container.ID, container.Config.MacAddress, "", "")
	if err != nil {
		return err
	}

	// Error handling: At this point, the interface is allocated so we have to
	// make sure that it is always released in case of error, otherwise we
	// might leak resources.

	if container.Config.PortSpecs != nil {
		if err = migratePortMappings(container.Config, container.hostConfig); err != nil {
			bridge.Release(container.ID)
			return err
		}
		container.Config.PortSpecs = nil
		if err = container.WriteHostConfig(); err != nil {
			bridge.Release(container.ID)
			return err
		}
	}

	var (
		portSpecs = make(nat.PortSet)
		bindings  = make(nat.PortMap)
	)

	if container.Config.ExposedPorts != nil {
		portSpecs = container.Config.ExposedPorts
	}

	if container.hostConfig.PortBindings != nil {
		for p, b := range container.hostConfig.PortBindings {
			bindings[p] = []nat.PortBinding{}
			for _, bb := range b {
				bindings[p] = append(bindings[p], nat.PortBinding{
					HostIp:   bb.HostIp,
					HostPort: bb.HostPort,
				})
			}
		}
	}

	container.NetworkSettings.PortMapping = nil

	for port := range portSpecs {
		if err = container.allocatePort(port, bindings); err != nil {
			bridge.Release(container.ID)
			return err
		}
	}
	container.WriteHostConfig()

	networkSettings.Ports = bindings
	container.NetworkSettings = networkSettings

	return nil
}

func (container *Container) ReleaseNetwork() {
	if container.Config.NetworkDisabled || !container.hostConfig.NetworkMode.IsPrivate() {
		return
	}

	bridge.Release(container.ID)

	container.NetworkSettings = &network.Settings{}
}

func (container *Container) isNetworkAllocated() bool {
	return container.NetworkSettings.IPAddress != ""
}

func (container *Container) RestoreNetwork() error {
	mode := container.hostConfig.NetworkMode
	// Don't attempt a restore if we previously didn't allocate networking.
	// This might be a legacy container with no network allocated, in which case the
	// allocation will happen once and for all at start.
	if !container.isNetworkAllocated() || container.Config.NetworkDisabled || !mode.IsPrivate() {
		return nil
	}

	// Re-allocate the interface with the same IP and MAC address.
	if _, err := bridge.Allocate(container.ID, container.NetworkSettings.MacAddress, container.NetworkSettings.IPAddress, ""); err != nil {
		return err
	}

	// Re-allocate any previously allocated ports.
	for port := range container.NetworkSettings.Ports {
		if err := container.allocatePort(port, container.NetworkSettings.Ports); err != nil {
			return err
		}
	}
	return nil
}

// cleanup releases any network resources allocated to the container along with any rules
// around how containers are linked together.  It also unmounts the container's root filesystem.
func (container *Container) cleanup() {
	container.ReleaseNetwork()

	// Disable all active links
	if container.activeLinks != nil {
		for _, link := range container.activeLinks {
			link.Disable()
		}
	}

	if err := container.Unmount(); err != nil {
		logrus.Errorf("%v: Failed to umount filesystem: %v", container.ID, err)
	}

	for _, eConfig := range container.execCommands.s {
		container.daemon.unregisterExecCommand(eConfig)
	}
}

func (container *Container) KillSig(sig int) error {
	logrus.Debugf("Sending %d to %s", sig, container.ID)
	container.Lock()
	defer container.Unlock()

	// We could unpause the container for them rather than returning this error
	if container.Paused {
		return fmt.Errorf("Container %s is paused. Unpause the container before stopping", container.ID)
	}

	if !container.Running {
		return nil
	}

	// signal to the monitor that it should not restart the container
	// after we send the kill signal
	container.monitor.ExitOnNext()

	// if the container is currently restarting we do not need to send the signal
	// to the process.  Telling the monitor that it should exit on it's next event
	// loop is enough
	if container.Restarting {
		return nil
	}

	return container.daemon.Kill(container, sig)
}

// Wrapper aroung KillSig() suppressing "no such process" error.
func (container *Container) killPossiblyDeadProcess(sig int) error {
	err := container.KillSig(sig)
	if err == syscall.ESRCH {
		logrus.Debugf("Cannot kill process (pid=%d) with signal %d: no such process.", container.GetPid(), sig)
		return nil
	}
	return err
}

func (container *Container) Pause() error {
	if container.IsPaused() {
		return fmt.Errorf("Container %s is already paused", container.ID)
	}
	if !container.IsRunning() {
		return fmt.Errorf("Container %s is not running", container.ID)
	}
	return container.daemon.Pause(container)
}

func (container *Container) Unpause() error {
	if !container.IsPaused() {
		return fmt.Errorf("Container %s is not paused", container.ID)
	}
	if !container.IsRunning() {
		return fmt.Errorf("Container %s is not running", container.ID)
	}
	return container.daemon.Unpause(container)
}

func (container *Container) Kill() error {
	if !container.IsRunning() {
		return nil
	}

	// 1. Send SIGKILL
	if err := container.killPossiblyDeadProcess(9); err != nil {
		return err
	}

	// 2. Wait for the process to die, in last resort, try to kill the process directly
	if _, err := container.WaitStop(10 * time.Second); err != nil {
		// Ensure that we don't kill ourselves
		if pid := container.GetPid(); pid != 0 {
			logrus.Infof("Container %s failed to exit within 10 seconds of kill - trying direct SIGKILL", stringid.TruncateID(container.ID))
			if err := syscall.Kill(pid, 9); err != nil {
				if err != syscall.ESRCH {
					return err
				}
				logrus.Debugf("Cannot kill process (pid=%d) with signal 9: no such process.", pid)
			}
		}
	}

	container.WaitStop(-1 * time.Second)
	return nil
}

func (container *Container) Stop(seconds int) error {
	if !container.IsRunning() {
		return nil
	}

	// 1. Send a SIGTERM
	if err := container.killPossiblyDeadProcess(15); err != nil {
		logrus.Infof("Failed to send SIGTERM to the process, force killing")
		if err := container.killPossiblyDeadProcess(9); err != nil {
			return err
		}
	}

	// 2. Wait for the process to exit on its own
	if _, err := container.WaitStop(time.Duration(seconds) * time.Second); err != nil {
		logrus.Infof("Container %v failed to exit within %d seconds of SIGTERM - using the force", container.ID, seconds)
		// 3. If it doesn't, then send SIGKILL
		if err := container.Kill(); err != nil {
			container.WaitStop(-1 * time.Second)
			return err
		}
	}
	return nil
}

func (container *Container) Restart(seconds int) error {
	// Avoid unnecessarily unmounting and then directly mounting
	// the container when the container stops and then starts
	// again
	if err := container.Mount(); err == nil {
		defer container.Unmount()
	}

	if err := container.Stop(seconds); err != nil {
		return err
	}
	return container.Start()
}

func (container *Container) Resize(h, w int) error {
	if !container.IsRunning() {
		return fmt.Errorf("Cannot resize container %s, container is not running", container.ID)
	}
	return container.command.ProcessConfig.Terminal.Resize(h, w)
}

func (container *Container) ExportRw() (archive.Archive, error) {
	if err := container.Mount(); err != nil {
		return nil, err
	}
	if container.daemon == nil {
		return nil, fmt.Errorf("Can't load storage driver for unregistered container %s", container.ID)
	}
	archive, err := container.daemon.Diff(container)
	if err != nil {
		container.Unmount()
		return nil, err
	}
	return ioutils.NewReadCloserWrapper(archive, func() error {
			err := archive.Close()
			container.Unmount()
			return err
		}),
		nil
}

func (container *Container) Export() (archive.Archive, error) {
	if err := container.Mount(); err != nil {
		return nil, err
	}

	archive, err := archive.Tar(container.basefs, archive.Uncompressed)
	if err != nil {
		container.Unmount()
		return nil, err
	}
	return ioutils.NewReadCloserWrapper(archive, func() error {
			err := archive.Close()
			container.Unmount()
			return err
		}),
		nil
}

func (container *Container) Mount() error {
	return container.daemon.Mount(container)
}

func (container *Container) changes() ([]archive.Change, error) {
	return container.daemon.Changes(container)
}

func (container *Container) Changes() ([]archive.Change, error) {
	container.Lock()
	defer container.Unlock()
	return container.changes()
}

func (container *Container) GetImage() (*image.Image, error) {
	if container.daemon == nil {
		return nil, fmt.Errorf("Can't get image of unregistered container")
	}
	return container.daemon.graph.Get(container.ImageID)
}

func (container *Container) Unmount() error {
	return container.daemon.Unmount(container)
}

func (container *Container) logPath(name string) (string, error) {
	return container.GetRootResourcePath(fmt.Sprintf("%s-%s.log", container.ID, name))
}

func (container *Container) ReadLog(name string) (io.Reader, error) {
	pth, err := container.logPath(name)
	if err != nil {
		return nil, err
	}
	return os.Open(pth)
}

func (container *Container) hostConfigPath() (string, error) {
	return container.GetRootResourcePath("hostconfig.json")
}

func (container *Container) jsonPath() (string, error) {
	return container.GetRootResourcePath("config.json")
}

// This method must be exported to be used from the lxc template
// This directory is only usable when the container is running
func (container *Container) RootfsPath() string {
	return container.basefs
}

func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("Invalid empty id")
	}
	return nil
}

// GetSize, return real size, virtual size
func (container *Container) GetSize() (int64, int64) {
	var (
		sizeRw, sizeRootfs int64
		err                error
		driver             = container.daemon.driver
	)

	if err := container.Mount(); err != nil {
		logrus.Errorf("Failed to compute size of container rootfs %s: %s", container.ID, err)
		return sizeRw, sizeRootfs
	}
	defer container.Unmount()

	initID := fmt.Sprintf("%s-init", container.ID)
	sizeRw, err = driver.DiffSize(container.ID, initID)
	if err != nil {
		logrus.Errorf("Driver %s couldn't return diff size of container %s: %s", driver, container.ID, err)
		// FIXME: GetSize should return an error. Not changing it now in case
		// there is a side-effect.
		sizeRw = -1
	}

	if _, err = os.Stat(container.basefs); err != nil {
		if sizeRootfs, err = directory.Size(container.basefs); err != nil {
			sizeRootfs = -1
		}
	}
	return sizeRw, sizeRootfs
}

func (container *Container) Copy(resource string) (io.ReadCloser, error) {
	container.Lock()
	defer container.Unlock()
	var err error
	if err := container.Mount(); err != nil {
		return nil, err
	}
	var paths []string
	unmount := func() {
		for _, p := range paths {
			syscall.Unmount(p, 0)
		}
	}
	defer func() {
		if err != nil {
			// unmount any volumes
			unmount()
			// unmount the container's rootfs
			container.Unmount()
		}
	}()
	mounts, err := container.setupMounts()
	if err != nil {
		return nil, err
	}
	for _, m := range mounts {
		dest, err := container.GetResourcePath(m.Destination)
		if err != nil {
			return nil, err
		}
		paths = append(paths, dest)
		if err := mount.Mount(m.Source, dest, "bind", "rbind,ro"); err != nil {
			return nil, err
		}
	}
	basePath, err := container.GetResourcePath(resource)
	if err != nil {
		return nil, err
	}
	stat, err := os.Stat(basePath)
	if err != nil {
		return nil, err
	}
	var filter []string
	if !stat.IsDir() {
		d, f := path.Split(basePath)
		basePath = d
		filter = []string{f}
	} else {
		filter = []string{path.Base(basePath)}
		basePath = path.Dir(basePath)
	}
	archive, err := archive.TarWithOptions(basePath, &archive.TarOptions{
		Compression:  archive.Uncompressed,
		IncludeFiles: filter,
	})
	if err != nil {
		return nil, err
	}
	return ioutils.NewReadCloserWrapper(archive, func() error {
			err := archive.Close()
			unmount()
			container.Unmount()
			return err
		}),
		nil
}

// Returns true if the container exposes a certain port
func (container *Container) Exposes(p nat.Port) bool {
	_, exists := container.Config.ExposedPorts[p]
	return exists
}

func (container *Container) HostConfig() *runconfig.HostConfig {
	container.Lock()
	res := container.hostConfig
	container.Unlock()
	return res
}

func (container *Container) SetHostConfig(hostConfig *runconfig.HostConfig) {
	container.Lock()
	container.hostConfig = hostConfig
	container.Unlock()
}

func (container *Container) DisableLink(name string) {
	if container.activeLinks != nil {
		if link, exists := container.activeLinks[name]; exists {
			link.Disable()
		} else {
			logrus.Debugf("Could not find active link for %s", name)
		}
	}
}

func (container *Container) setupContainerDns() error {
	if container.ResolvConfPath != "" {
		// check if this is an existing container that needs DNS update:
		if container.UpdateDns {
			// read the host's resolv.conf, get the hash and call updateResolvConf
			logrus.Debugf("Check container (%s) for update to resolv.conf - UpdateDns flag was set", container.ID)
			latestResolvConf, latestHash := resolvconf.GetLastModified()

			// clean container resolv.conf re: localhost nameservers and IPv6 NS (if IPv6 disabled)
			updatedResolvConf, modified := resolvconf.FilterResolvDns(latestResolvConf, container.daemon.config.Bridge.EnableIPv6)
			if modified {
				// changes have occurred during resolv.conf localhost cleanup: generate an updated hash
				newHash, err := ioutils.HashData(bytes.NewReader(updatedResolvConf))
				if err != nil {
					return err
				}
				latestHash = newHash
			}

			if err := container.updateResolvConf(updatedResolvConf, latestHash); err != nil {
				return err
			}
			// successful update of the restarting container; set the flag off
			container.UpdateDns = false
		}
		return nil
	}

	var (
		config = container.hostConfig
		daemon = container.daemon
	)

	resolvConf, err := resolvconf.Get()
	if err != nil {
		return err
	}
	container.ResolvConfPath, err = container.GetRootResourcePath("resolv.conf")
	if err != nil {
		return err
	}

	if config.NetworkMode != "host" {
		// check configurations for any container/daemon dns settings
		if len(config.Dns) > 0 || len(daemon.config.Dns) > 0 || len(config.DnsSearch) > 0 || len(daemon.config.DnsSearch) > 0 {
			var (
				dns       = resolvconf.GetNameservers(resolvConf)
				dnsSearch = resolvconf.GetSearchDomains(resolvConf)
			)
			if len(config.Dns) > 0 {
				dns = config.Dns
			} else if len(daemon.config.Dns) > 0 {
				dns = daemon.config.Dns
			}
			if len(config.DnsSearch) > 0 {
				dnsSearch = config.DnsSearch
			} else if len(daemon.config.DnsSearch) > 0 {
				dnsSearch = daemon.config.DnsSearch
			}
			return resolvconf.Build(container.ResolvConfPath, dns, dnsSearch)
		}

		// replace any localhost/127.*, and remove IPv6 nameservers if IPv6 disabled in daemon
		resolvConf, _ = resolvconf.FilterResolvDns(resolvConf, daemon.config.Bridge.EnableIPv6)
	}
	//get a sha256 hash of the resolv conf at this point so we can check
	//for changes when the host resolv.conf changes (e.g. network update)
	resolvHash, err := ioutils.HashData(bytes.NewReader(resolvConf))
	if err != nil {
		return err
	}
	resolvHashFile := container.ResolvConfPath + ".hash"
	if err = ioutil.WriteFile(resolvHashFile, []byte(resolvHash), 0644); err != nil {
		return err
	}
	return ioutil.WriteFile(container.ResolvConfPath, resolvConf, 0644)
}

// called when the host's resolv.conf changes to check whether container's resolv.conf
// is unchanged by the container "user" since container start: if unchanged, the
// container's resolv.conf will be updated to match the host's new resolv.conf
func (container *Container) updateResolvConf(updatedResolvConf []byte, newResolvHash string) error {

	if container.ResolvConfPath == "" {
		return nil
	}
	if container.Running {
		//set a marker in the hostConfig to update on next start/restart
		container.UpdateDns = true
		return nil
	}

	resolvHashFile := container.ResolvConfPath + ".hash"

	//read the container's current resolv.conf and compute the hash
	resolvBytes, err := ioutil.ReadFile(container.ResolvConfPath)
	if err != nil {
		return err
	}
	curHash, err := ioutils.HashData(bytes.NewReader(resolvBytes))
	if err != nil {
		return err
	}

	//read the hash from the last time we wrote resolv.conf in the container
	hashBytes, err := ioutil.ReadFile(resolvHashFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// backwards compat: if no hash file exists, this container pre-existed from
		// a Docker daemon that didn't contain this update feature. Given we can't know
		// if the user has modified the resolv.conf since container start time, safer
		// to just never update the container's resolv.conf during it's lifetime which
		// we can control by setting hashBytes to an empty string
		hashBytes = []byte("")
	}

	//if the user has not modified the resolv.conf of the container since we wrote it last
	//we will replace it with the updated resolv.conf from the host
	if string(hashBytes) == curHash {
		logrus.Debugf("replacing %q with updated host resolv.conf", container.ResolvConfPath)

		// for atomic updates to these files, use temporary files with os.Rename:
		dir := path.Dir(container.ResolvConfPath)
		tmpHashFile, err := ioutil.TempFile(dir, "hash")
		if err != nil {
			return err
		}
		tmpResolvFile, err := ioutil.TempFile(dir, "resolv")
		if err != nil {
			return err
		}

		// write the updates to the temp files
		if err = ioutil.WriteFile(tmpHashFile.Name(), []byte(newResolvHash), 0644); err != nil {
			return err
		}
		if err = ioutil.WriteFile(tmpResolvFile.Name(), updatedResolvConf, 0644); err != nil {
			return err
		}

		// rename the temp files for atomic replace
		if err = os.Rename(tmpHashFile.Name(), resolvHashFile); err != nil {
			return err
		}
		return os.Rename(tmpResolvFile.Name(), container.ResolvConfPath)
	}
	return nil
}

func (container *Container) updateParentsHosts() error {
	refs := container.daemon.ContainerGraph().RefPaths(container.ID)
	for _, ref := range refs {
		if ref.ParentID == "0" {
			continue
		}

		c, err := container.daemon.Get(ref.ParentID)
		if err != nil {
			logrus.Error(err)
		}

		if c != nil && !container.daemon.config.DisableNetwork && container.hostConfig.NetworkMode.IsPrivate() {
			logrus.Debugf("Update /etc/hosts of %s for alias %s with ip %s", c.ID, ref.Name, container.NetworkSettings.IPAddress)
			if err := etchosts.Update(c.HostsPath, container.NetworkSettings.IPAddress, ref.Name); err != nil {
				logrus.Errorf("Failed to update /etc/hosts in parent container %s for alias %s: %v", c.ID, ref.Name, err)
			}
		}
	}
	return nil
}

func (container *Container) initializeNetworking() error {
	var err error
	if container.hostConfig.NetworkMode.IsHost() {
		container.Config.Hostname, err = os.Hostname()
		if err != nil {
			return err
		}

		parts := strings.SplitN(container.Config.Hostname, ".", 2)
		if len(parts) > 1 {
			container.Config.Hostname = parts[0]
			container.Config.Domainname = parts[1]
		}

		content, err := ioutil.ReadFile("/etc/hosts")
		if os.IsNotExist(err) {
			return container.buildHostnameAndHostsFiles("")
		} else if err != nil {
			return err
		}

		if err := container.buildHostnameFile(); err != nil {
			return err
		}

		hostsPath, err := container.GetRootResourcePath("hosts")
		if err != nil {
			return err
		}
		container.HostsPath = hostsPath

		return ioutil.WriteFile(container.HostsPath, content, 0644)
	}
	if container.hostConfig.NetworkMode.IsContainer() {
		// we need to get the hosts files from the container to join
		nc, err := container.getNetworkedContainer()
		if err != nil {
			return err
		}
		container.HostnamePath = nc.HostnamePath
		container.HostsPath = nc.HostsPath
		container.ResolvConfPath = nc.ResolvConfPath
		container.Config.Hostname = nc.Config.Hostname
		container.Config.Domainname = nc.Config.Domainname
		return nil
	}
	if container.daemon.config.DisableNetwork {
		container.Config.NetworkDisabled = true
		return container.buildHostnameAndHostsFiles("127.0.1.1")
	}
	if err := container.AllocateNetwork(); err != nil {
		return err
	}
	return container.buildHostnameAndHostsFiles(container.NetworkSettings.IPAddress)
}

// Make sure the config is compatible with the current kernel
func (container *Container) verifyDaemonSettings() {
	if container.hostConfig.Memory > 0 && !container.daemon.sysInfo.MemoryLimit {
		logrus.Warnf("Your kernel does not support memory limit capabilities. Limitation discarded.")
		container.hostConfig.Memory = 0
	}
	if container.hostConfig.Memory > 0 && container.hostConfig.MemorySwap != -1 && !container.daemon.sysInfo.SwapLimit {
		logrus.Warnf("Your kernel does not support swap limit capabilities. Limitation discarded.")
		container.hostConfig.MemorySwap = -1
	}
	if container.daemon.sysInfo.IPv4ForwardingDisabled {
		logrus.Warnf("IPv4 forwarding is disabled. Networking will not work")
	}
}

func (container *Container) setupLinkedContainers() ([]string, error) {
	var (
		env    []string
		daemon = container.daemon
	)
	children, err := daemon.Children(container.Name)
	if err != nil {
		return nil, err
	}

	if len(children) > 0 {
		container.activeLinks = make(map[string]*links.Link, len(children))

		// If we encounter an error make sure that we rollback any network
		// config and iptables changes
		rollback := func() {
			for _, link := range container.activeLinks {
				link.Disable()
			}
			container.activeLinks = nil
		}

		for linkAlias, child := range children {
			if !child.IsRunning() {
				return nil, fmt.Errorf("Cannot link to a non running container: %s AS %s", child.Name, linkAlias)
			}

			link, err := links.NewLink(
				container.NetworkSettings.IPAddress,
				child.NetworkSettings.IPAddress,
				linkAlias,
				child.Config.Env,
				child.Config.ExposedPorts,
			)

			if err != nil {
				rollback()
				return nil, err
			}

			container.activeLinks[link.Alias()] = link
			if err := link.Enable(); err != nil {
				rollback()
				return nil, err
			}

			for _, envVar := range link.ToEnv() {
				env = append(env, envVar)
			}
		}
	}
	return env, nil
}

func (container *Container) createDaemonEnvironment(linkedEnv []string) []string {
	// if a domain name was specified, append it to the hostname (see #7851)
	fullHostname := container.Config.Hostname
	if container.Config.Domainname != "" {
		fullHostname = fmt.Sprintf("%s.%s", fullHostname, container.Config.Domainname)
	}
	// Setup environment
	env := []string{
		"PATH=" + DefaultPathEnv,
		"HOSTNAME=" + fullHostname,
		// Note: we don't set HOME here because it'll get autoset intelligently
		// based on the value of USER inside dockerinit, but only if it isn't
		// set already (ie, that can be overridden by setting HOME via -e or ENV
		// in a Dockerfile).
	}
	if container.Config.Tty {
		env = append(env, "TERM=xterm")
	}
	env = append(env, linkedEnv...)
	// because the env on the container can override certain default values
	// we need to replace the 'env' keys where they match and append anything
	// else.
	env = utils.ReplaceOrAppendEnvValues(env, container.Config.Env)

	return env
}

func (container *Container) setupWorkingDirectory() error {
	if container.Config.WorkingDir != "" {
		container.Config.WorkingDir = path.Clean(container.Config.WorkingDir)

		pth, err := container.GetResourcePath(container.Config.WorkingDir)
		if err != nil {
			return err
		}

		pthInfo, err := os.Stat(pth)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}

			if err := os.MkdirAll(pth, 0755); err != nil {
				return err
			}
		}
		if pthInfo != nil && !pthInfo.IsDir() {
			return fmt.Errorf("Cannot mkdir: %s is not a directory", container.Config.WorkingDir)
		}
	}
	return nil
}

func (container *Container) startLogging() error {
	cfg := container.hostConfig.LogConfig
	if cfg.Type == "" {
		cfg = container.daemon.defaultLogConfig
	}
	var l logger.Logger
	switch cfg.Type {
	case "json-file":
		pth, err := container.logPath("json")
		if err != nil {
			return err
		}
		container.LogPath = pth

		dl, err := jsonfilelog.New(pth)
		if err != nil {
			return err
		}
		l = dl
	case "syslog":
		dl, err := syslog.New(container.ID[:12])
		if err != nil {
			return err
		}
		l = dl
	case "journald":
		dl, err := journald.New(container.ID, container.Name)
		if err != nil {
			return err
		}
		l = dl
	case "none":
		return nil
	default:
		return fmt.Errorf("Unknown logging driver: %s", cfg.Type)
	}

	copier, err := logger.NewCopier(container.ID, map[string]io.Reader{"stdout": container.StdoutPipe(), "stderr": container.StderrPipe()}, l)
	if err != nil {
		return err
	}
	container.logCopier = copier
	copier.Run()
	container.logDriver = l

	return nil
}

func (container *Container) waitForStart() error {
	container.monitor = newContainerMonitor(container, container.hostConfig.RestartPolicy)

	// block until we either receive an error from the initial start of the container's
	// process or until the process is running in the container
	select {
	case <-container.monitor.startSignal:
	case err := <-promise.Go(container.monitor.Start):
		return err
	}

	return nil
}

func (container *Container) allocatePort(port nat.Port, bindings nat.PortMap) error {
	binding := bindings[port]
	if container.hostConfig.PublishAllPorts && len(binding) == 0 {
		binding = append(binding, nat.PortBinding{})
	}

	for i := 0; i < len(binding); i++ {
		b, err := bridge.AllocatePort(container.ID, port, binding[i])
		if err != nil {
			return err
		}
		binding[i] = b
	}
	bindings[port] = binding
	return nil
}

func (container *Container) GetProcessLabel() string {
	// even if we have a process label return "" if we are running
	// in privileged mode
	if container.hostConfig.Privileged {
		return ""
	}
	return container.ProcessLabel
}

func (container *Container) GetMountLabel() string {
	if container.hostConfig.Privileged {
		return ""
	}
	return container.MountLabel
}

func (container *Container) getIpcContainer() (*Container, error) {
	containerID := container.hostConfig.IpcMode.Container()
	c, err := container.daemon.Get(containerID)
	if err != nil {
		return nil, err
	}
	if !c.IsRunning() {
		return nil, fmt.Errorf("cannot join IPC of a non running container: %s", containerID)
	}
	return c, nil
}

func (container *Container) getNetworkedContainer() (*Container, error) {
	parts := strings.SplitN(string(container.hostConfig.NetworkMode), ":", 2)
	switch parts[0] {
	case "container":
		if len(parts) != 2 {
			return nil, fmt.Errorf("no container specified to join network")
		}
		nc, err := container.daemon.Get(parts[1])
		if err != nil {
			return nil, err
		}
		if container == nc {
			return nil, fmt.Errorf("cannot join own network")
		}
		if !nc.IsRunning() {
			return nil, fmt.Errorf("cannot join network of a non running container: %s", parts[1])
		}
		return nc, nil
	default:
		return nil, fmt.Errorf("network mode not set to container")
	}
}

func (container *Container) Stats() (*execdriver.ResourceStats, error) {
	return container.daemon.Stats(container)
}

func (c *Container) LogDriverType() string {
	c.Lock()
	defer c.Unlock()
	if c.hostConfig.LogConfig.Type == "" {
		return c.daemon.defaultLogConfig.Type
	}
	return c.hostConfig.LogConfig.Type
}

func (container *Container) GetExecIDs() []string {
	return container.execCommands.List()
}

func (container *Container) Exec(execConfig *execConfig) error {
	container.Lock()
	defer container.Unlock()

	waitStart := make(chan struct{})

	callback := func(processConfig *execdriver.ProcessConfig, pid int) {
		if processConfig.Tty {
			// The callback is called after the process Start()
			// so we are in the parent process. In TTY mode, stdin/out/err is the PtySlave
			// which we close here.
			if c, ok := processConfig.Stdout.(io.Closer); ok {
				c.Close()
			}
		}
		close(waitStart)
	}

	// We use a callback here instead of a goroutine and an chan for
	// syncronization purposes
	cErr := promise.Go(func() error { return container.monitorExec(execConfig, callback) })

	// Exec should not return until the process is actually running
	select {
	case <-waitStart:
	case err := <-cErr:
		return err
	}

	return nil
}

func (container *Container) monitorExec(execConfig *execConfig, callback execdriver.StartCallback) error {
	var (
		err      error
		exitCode int
	)

	pipes := execdriver.NewPipes(execConfig.StreamConfig.stdin, execConfig.StreamConfig.stdout, execConfig.StreamConfig.stderr, execConfig.OpenStdin)
	exitCode, err = container.daemon.Exec(container, execConfig, pipes, callback)
	if err != nil {
		logrus.Errorf("Error running command in existing container %s: %s", container.ID, err)
	}

	logrus.Debugf("Exec task in container %s exited with code %d", container.ID, exitCode)
	if execConfig.OpenStdin {
		if err := execConfig.StreamConfig.stdin.Close(); err != nil {
			logrus.Errorf("Error closing stdin while running in %s: %s", container.ID, err)
		}
	}
	if err := execConfig.StreamConfig.stdout.Clean(); err != nil {
		logrus.Errorf("Error closing stdout while running in %s: %s", container.ID, err)
	}
	if err := execConfig.StreamConfig.stderr.Clean(); err != nil {
		logrus.Errorf("Error closing stderr while running in %s: %s", container.ID, err)
	}
	if execConfig.ProcessConfig.Terminal != nil {
		if err := execConfig.ProcessConfig.Terminal.Close(); err != nil {
			logrus.Errorf("Error closing terminal while running in container %s: %s", container.ID, err)
		}
	}

	return err
}

func (container *Container) setupMounts() ([]execdriver.Mount, error) {
	var mounts []execdriver.Mount
	for _, v := range container.volumes {
		config, ok := container.VolumeConfig[v.Name()]
		if !ok {
			return nil, fmt.Errorf("volume configuration not found for %s", v.Name())
		}
		path, err := v.Mount()
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, execdriver.Mount{
			Source:      path,
			Destination: config.Destination,
			Writable:    config.RW,
		})
	}
	for _, b := range container.BindMounts {
		mounts = append(mounts, execdriver.Mount{
			Source:      b.Source,
			Destination: b.Destination,
			Writable:    b.RW,
		})
	}
	mounts = sortMounts(mounts)
	return append(mounts, container.networkMounts()...), nil
}

func (container *Container) networkMounts() []execdriver.Mount {
	var mounts []execdriver.Mount
	if container.ResolvConfPath != "" {
		mounts = append(mounts, execdriver.Mount{
			Source:      container.ResolvConfPath,
			Destination: "/etc/resolv.conf",
			Writable:    !container.hostConfig.ReadonlyRootfs,
			Private:     true,
		})
	}
	if container.HostnamePath != "" {
		mounts = append(mounts, execdriver.Mount{
			Source:      container.HostnamePath,
			Destination: "/etc/hostname",
			Writable:    !container.hostConfig.ReadonlyRootfs,
			Private:     true,
		})
	}
	if container.HostsPath != "" {
		mounts = append(mounts, execdriver.Mount{
			Source:      container.HostsPath,
			Destination: "/etc/hosts",
			Writable:    !container.hostConfig.ReadonlyRootfs,
			Private:     true,
		})
	}
	return mounts
}

func sortMounts(m []execdriver.Mount) []execdriver.Mount {
	sort.Sort(mounts(m))
	return m
}

type mounts []execdriver.Mount

func (m mounts) Len() int {
	return len(m)
}

func (m mounts) Less(i, j int) bool {
	return m.parts(i) < m.parts(j)
}

func (m mounts) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

func (m mounts) parts(i int) int {
	return len(strings.Split(filepath.Clean(m[i].Destination), string(os.PathSeparator)))
}
