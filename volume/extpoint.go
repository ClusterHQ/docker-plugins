package volume

import "sync"

// currently created by hand. generation tool would generate this like:
// $ extpoint-gen volume/volume.go Driver > volume/extpoint.go

var Drivers = &driverExtpoint{extensions: make(map[string]Driver)}

type driverExtpoint struct {
	extensions map[string]Driver
	sync.Mutex
}

func (ep *driverExtpoint) Register(extension Driver, name string) bool {
	ep.Lock()
	defer ep.Unlock()
	if name == "" {
		return false
	}
	_, exists := ep.extensions[name]
	if exists {
		return false
	}
	ep.extensions[name] = extension
	return true
}

func (ep *driverExtpoint) Unregister(name string) bool {
	ep.Lock()
	defer ep.Unlock()
	_, exists := ep.extensions[name]
	if !exists {
		return false
	}
	delete(ep.extensions, name)
	return true
}

func (ep *driverExtpoint) Lookup(name string) Driver {
	ep.Lock()
	defer ep.Unlock()
	ext, ok := ep.extensions[name]
	if !ok {
		return nil
	}
	return ext
}

func (ep *driverExtpoint) All() map[string]Driver {
	ep.Lock()
	defer ep.Unlock()
	all := make(map[string]Driver)
	for k, v := range ep.extensions {
		all[k] = v
	}
	return all
}

func (ep *driverExtpoint) Select(names []string) []Driver {
	var selected []Driver
	for _, name := range names {
		selected = append(selected, ep.Lookup(name))
	}
	return selected
}

func (ep *driverExtpoint) Names() []string {
	var names []string
	for k := range ep.All() {
		names = append(names, k)
	}
	return names
}
