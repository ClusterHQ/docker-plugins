package volumedrivers

import (
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/plugins"
	"github.com/docker/docker/volume"
)

// currently created by hand. generation tool would generate this like:
// $ extpoint-gen Driver > volume/extpoint.go

var drivers = &driverExtpoint{extensions: make(map[string]volume.Driver)}

type driverExtpoint struct {
	extensions map[string]volume.Driver
	sync.Mutex
}

func Register(extension volume.Driver, name string) bool {
	drivers.Lock()
	defer drivers.Unlock()
	if name == "" {
		return false
	}
	_, exists := drivers.extensions[name]
	if exists {
		return false
	}
	drivers.extensions[name] = extension
	return true
}

func Unregister(name string) bool {
	drivers.Lock()
	defer drivers.Unlock()
	_, exists := drivers.extensions[name]
	if !exists {
		return false
	}
	delete(drivers.extensions, name)
	return true
}

func Lookup(name string) volume.Driver {
	drivers.Lock()
	defer drivers.Unlock()
	ext, ok := drivers.extensions[name]
	if ok {
		return ext
	}
	pl, err := plugins.Get(name, "VolumeDriver")
	if err != nil {
		logrus.Errorf("Error: %v", err)
		return nil
	}
	d := NewVolumeDriver(name, pl.Client)
	drivers.extensions[name] = d
	return d
}
