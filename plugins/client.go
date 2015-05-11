package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/docker/docker/utils"
)

const (
	versionMimetype = "appplication/vnd.docker.plugins.v1+json"
)

func NewClient(addr string) *Client {
	// No TLS. Hopefully this discourages non-local plugins
	tr := &http.Transport{}
	protoAndAddr := strings.Split(addr, "://")
	utils.ConfigureTCPTransport(tr, protoAndAddr[0], protoAndAddr[1])
	return &Client{&http.Client{Transport: tr}, protoAndAddr[1]}
}

type Client struct {
	http *http.Client
	addr string
}

func (c *Client) Call(serviceMethod string, args interface{}, ret interface{}) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(args); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "/"+serviceMethod, &buf)
	req.Header.Add("Accept", versionMimetype)
	req.URL.Scheme = "http"
	req.URL.Host = c.addr
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		remoteErr, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil
		}
		return fmt.Errorf("Plugin Error: %s", remoteErr)
	}

	if err := json.NewDecoder(resp.Body).Decode(&ret); err != nil {
		return err
	}
	return nil
}
