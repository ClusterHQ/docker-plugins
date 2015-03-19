package plugins

import (
	"encoding/json"
	"io"
)

type Plugin struct {
	addr string
	kind string
}

type handshakeResp struct {
	InterestedIn []string
	Name         string
	Author       string
	Org          string
	Website      string
}

func (p *Plugin) Call(method, path string, data interface{}) (io.ReadCloser, error) {
	path = "/" + p.kind + "/" + path
	return call(p.addr, method, path, data)
}

func (p *Plugin) handshake() (*handshakeResp, error) {
	// Don't use the local `call` because this shouldn't be namespaced
	respBody, err := call(p.addr, "POST", "handshake", nil)
	if err != nil {
		return nil, err
	}
	defer respBody.Close()

	var data handshakeResp
	return &data, json.NewDecoder(respBody).Decode(&data)
}
