package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/stats"
	"github.com/docker/docker/vendor/src/code.google.com/p/go/src/pkg/archive/tar"
)

func TestContainerApiGetAll(t *testing.T) {
	defer deleteAllContainers()

	startCount, err := getContainerCount()
	if err != nil {
		t.Fatalf("Cannot query container count: %v", err)
	}

	name := "getall"
	runCmd := exec.Command(dockerBinary, "run", "--name", name, "busybox", "true")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		t.Fatalf("Error on container creation: %v, output: %q", err, out)
	}

	body, err := sockRequest("GET", "/containers/json?all=1", nil)
	if err != nil {
		t.Fatalf("GET all containers sockRequest failed: %v", err)
	}

	var inspectJSON []struct {
		Names []string
	}
	if err = json.Unmarshal(body, &inspectJSON); err != nil {
		t.Fatalf("unable to unmarshal response body: %v", err)
	}

	if len(inspectJSON) != startCount+1 {
		t.Fatalf("Expected %d container(s), %d found (started with: %d)", startCount+1, len(inspectJSON), startCount)
	}

	if actual := inspectJSON[0].Names[0]; actual != "/"+name {
		t.Fatalf("Container Name mismatch. Expected: %q, received: %q\n", "/"+name, actual)
	}

	logDone("container REST API - check GET json/all=1")
}

func TestContainerApiGetExport(t *testing.T) {
	defer deleteAllContainers()

	name := "exportcontainer"
	runCmd := exec.Command(dockerBinary, "run", "--name", name, "busybox", "touch", "/test")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		t.Fatalf("Error on container creation: %v, output: %q", err, out)
	}

	body, err := sockRequest("GET", "/containers/"+name+"/export", nil)
	if err != nil {
		t.Fatalf("GET containers/export sockRequest failed: %v", err)
	}

	found := false
	for tarReader := tar.NewReader(bytes.NewReader(body)); ; {
		h, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if h.Name == "test" {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("The created test file has not been found in the exported image")
	}

	logDone("container REST API - check GET containers/export")
}

func TestContainerApiGetChanges(t *testing.T) {
	defer deleteAllContainers()

	name := "changescontainer"
	runCmd := exec.Command(dockerBinary, "run", "--name", name, "busybox", "rm", "/etc/passwd")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		t.Fatalf("Error on container creation: %v, output: %q", err, out)
	}

	body, err := sockRequest("GET", "/containers/"+name+"/changes", nil)
	if err != nil {
		t.Fatalf("GET containers/changes sockRequest failed: %v", err)
	}

	changes := []struct {
		Kind int
		Path string
	}{}
	if err = json.Unmarshal(body, &changes); err != nil {
		t.Fatalf("unable to unmarshal response body: %v", err)
	}

	// Check the changelog for removal of /etc/passwd
	success := false
	for _, elem := range changes {
		if elem.Path == "/etc/passwd" && elem.Kind == 2 {
			success = true
		}
	}
	if !success {
		t.Fatalf("/etc/passwd has been removed but is not present in the diff")
	}

	logDone("container REST API - check GET containers/changes")
}

func TestContainerApiStartVolumeBinds(t *testing.T) {
	defer deleteAllContainers()
	name := "testing"
	config := map[string]interface{}{
		"Image":   "busybox",
		"Volumes": map[string]struct{}{"/tmp": {}},
	}

	if _, err := sockRequest("POST", "/containers/create?name="+name, config); err != nil && !strings.Contains(err.Error(), "201 Created") {
		t.Fatal(err)
	}

	bindPath := randomUnixTmpDirPath("test")
	config = map[string]interface{}{
		"Binds": []string{bindPath + ":/tmp"},
	}
	if _, err := sockRequest("POST", "/containers/"+name+"/start", config); err != nil && !strings.Contains(err.Error(), "204 No Content") {
		t.Fatal(err)
	}

	pth, err := inspectFieldMap(name, "Volumes", "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	if pth != bindPath {
		t.Fatalf("expected volume host path to be %s, got %s", bindPath, pth)
	}

	logDone("container REST API - check volume binds on start")
}

// Test for GH#10618
func TestContainerApiStartDupVolumeBinds(t *testing.T) {
	defer deleteAllContainers()
	name := "testdups"
	config := map[string]interface{}{
		"Image":   "busybox",
		"Volumes": map[string]struct{}{"/tmp": {}},
	}

	if _, err := sockRequest("POST", "/containers/create?name="+name, config); err != nil && !strings.Contains(err.Error(), "201 Created") {
		t.Fatal(err)
	}

	bindPath1 := randomUnixTmpDirPath("test1")
	bindPath2 := randomUnixTmpDirPath("test2")

	config = map[string]interface{}{
		"Binds": []string{bindPath1 + ":/tmp", bindPath2 + ":/tmp"},
	}
	if body, err := sockRequest("POST", "/containers/"+name+"/start", config); err == nil {
		t.Fatal("expected container start to fail when duplicate volume binds to same container path")
	} else {
		if !strings.Contains(string(body), "Duplicate volume") {
			t.Fatalf("Expected failure due to duplicate bind mounts to same path, instead got: %q with error: %v", string(body), err)
		}
	}

	logDone("container REST API - check for duplicate volume binds error on start")
}
func TestContainerApiStartVolumesFrom(t *testing.T) {
	defer deleteAllContainers()
	volName := "voltst"
	volPath := "/tmp"

	if out, _, err := runCommandWithOutput(exec.Command(dockerBinary, "run", "-d", "--name", volName, "-v", volPath, "busybox")); err != nil {
		t.Fatal(out, err)
	}

	name := "testing"
	config := map[string]interface{}{
		"Image":   "busybox",
		"Volumes": map[string]struct{}{volPath: {}},
	}

	if _, err := sockRequest("POST", "/containers/create?name="+name, config); err != nil && !strings.Contains(err.Error(), "201 Created") {
		t.Fatal(err)
	}

	config = map[string]interface{}{
		"VolumesFrom": []string{volName},
	}
	if _, err := sockRequest("POST", "/containers/"+name+"/start", config); err != nil && !strings.Contains(err.Error(), "204 No Content") {
		t.Fatal(err)
	}

	pth, err := inspectFieldMap(name, "Volumes", volPath)
	if err != nil {
		t.Fatal(err)
	}
	pth2, err := inspectFieldMap(volName, "Volumes", volPath)
	if err != nil {
		t.Fatal(err)
	}

	if pth != pth2 {
		t.Fatalf("expected volume host path to be %s, got %s", pth, pth2)
	}

	logDone("container REST API - check VolumesFrom on start")
}

// Ensure that volumes-from has priority over binds/anything else
// This is pretty much the same as TestRunApplyVolumesFromBeforeVolumes, except with passing the VolumesFrom and the bind on start
func TestVolumesFromHasPriority(t *testing.T) {
	defer deleteAllContainers()
	volName := "voltst"
	volPath := "/tmp"

	if out, _, err := runCommandWithOutput(exec.Command(dockerBinary, "run", "-d", "--name", volName, "-v", volPath, "busybox")); err != nil {
		t.Fatal(out, err)
	}

	name := "testing"
	config := map[string]interface{}{
		"Image":   "busybox",
		"Volumes": map[string]struct{}{volPath: {}},
	}

	if _, err := sockRequest("POST", "/containers/create?name="+name, config); err != nil && !strings.Contains(err.Error(), "201 Created") {
		t.Fatal(err)
	}

	bindPath := randomUnixTmpDirPath("test")
	config = map[string]interface{}{
		"VolumesFrom": []string{volName},
		"Binds":       []string{bindPath + ":/tmp"},
	}
	if _, err := sockRequest("POST", "/containers/"+name+"/start", config); err != nil && !strings.Contains(err.Error(), "204 No Content") {
		t.Fatal(err)
	}

	pth, err := inspectFieldMap(name, "Volumes", volPath)
	if err != nil {
		t.Fatal(err)
	}
	pth2, err := inspectFieldMap(volName, "Volumes", volPath)
	if err != nil {
		t.Fatal(err)
	}

	if pth != pth2 {
		t.Fatalf("expected volume host path to be %s, got %s", pth, pth2)
	}

	logDone("container REST API - check VolumesFrom has priority")
}

func TestGetContainerStats(t *testing.T) {
	defer deleteAllContainers()
	var (
		name   = "statscontainer"
		runCmd = exec.Command(dockerBinary, "run", "-d", "--name", name, "busybox", "top")
	)
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		t.Fatalf("Error on container creation: %v, output: %q", err, out)
	}
	type b struct {
		body []byte
		err  error
	}
	bc := make(chan b, 1)
	go func() {
		body, err := sockRequest("GET", "/containers/"+name+"/stats", nil)
		bc <- b{body, err}
	}()

	// allow some time to stream the stats from the container
	time.Sleep(4 * time.Second)
	if _, err := runCommand(exec.Command(dockerBinary, "rm", "-f", name)); err != nil {
		t.Fatal(err)
	}

	// collect the results from the stats stream or timeout and fail
	// if the stream was not disconnected.
	select {
	case <-time.After(2 * time.Second):
		t.Fatal("stream was not closed after container was removed")
	case sr := <-bc:
		if sr.err != nil {
			t.Fatal(err)
		}

		dec := json.NewDecoder(bytes.NewBuffer(sr.body))
		var s *stats.Stats
		// decode only one object from the stream
		if err := dec.Decode(&s); err != nil {
			t.Fatal(err)
		}
	}
	logDone("container REST API - check GET containers/stats")
}

func TestBuildApiDockerfilePath(t *testing.T) {
	// Test to make sure we stop people from trying to leave the
	// build context when specifying the path to the dockerfile
	buffer := new(bytes.Buffer)
	tw := tar.NewWriter(buffer)
	defer tw.Close()

	dockerfile := []byte("FROM busybox")
	if err := tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Size: int64(len(dockerfile)),
	}); err != nil {
		t.Fatalf("failed to write tar file header: %v", err)
	}
	if _, err := tw.Write(dockerfile); err != nil {
		t.Fatalf("failed to write tar file content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar archive: %v", err)
	}

	out, err := sockRequestRaw("POST", "/build?dockerfile=../Dockerfile", buffer, "application/x-tar")
	if err == nil {
		t.Fatalf("Build was supposed to fail: %s", out)
	}

	if !strings.Contains(string(out), "must be within the build context") {
		t.Fatalf("Didn't complain about leaving build context: %s", out)
	}

	logDone("container REST API - check build w/bad Dockerfile path")
}

func TestBuildApiDockerfileSymlink(t *testing.T) {
	// Test to make sure we stop people from trying to leave the
	// build context when specifying a symlink as the path to the dockerfile
	buffer := new(bytes.Buffer)
	tw := tar.NewWriter(buffer)
	defer tw.Close()

	if err := tw.WriteHeader(&tar.Header{
		Name:     "Dockerfile",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	}); err != nil {
		t.Fatalf("failed to write tar file header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close tar archive: %v", err)
	}

	out, err := sockRequestRaw("POST", "/build", buffer, "application/x-tar")
	if err == nil {
		t.Fatalf("Build was supposed to fail: %s", out)
	}

	// The reason the error is "Cannot locate specified Dockerfile" is because
	// in the builder, the symlink is resolved within the context, therefore
	// Dockerfile -> /etc/passwd becomes etc/passwd from the context which is
	// a nonexistent file.
	if !strings.Contains(string(out), "Cannot locate specified Dockerfile: Dockerfile") {
		t.Fatalf("Didn't complain about leaving build context: %s", out)
	}

	logDone("container REST API - check build w/bad Dockerfile symlink path")
}

// #9981 - Allow a docker created volume (ie, one in /var/lib/docker/volumes) to be used to overwrite (via passing in Binds on api start) an existing volume
func TestPostContainerBindNormalVolume(t *testing.T) {
	defer deleteAllContainers()

	out, _, err := runCommandWithOutput(exec.Command(dockerBinary, "create", "-v", "/foo", "--name=one", "busybox"))
	if err != nil {
		t.Fatal(err, out)
	}

	fooDir, err := inspectFieldMap("one", "Volumes", "/foo")
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = runCommandWithOutput(exec.Command(dockerBinary, "create", "-v", "/foo", "--name=two", "busybox"))
	if err != nil {
		t.Fatal(err, out)
	}

	bindSpec := map[string][]string{"Binds": {fooDir + ":/foo"}}
	_, err = sockRequest("POST", "/containers/two/start", bindSpec)
	if err != nil && !strings.Contains(err.Error(), "204 No Content") {
		t.Fatal(err)
	}

	fooDir2, err := inspectFieldMap("two", "Volumes", "/foo")
	if err != nil {
		t.Fatal(err)
	}

	if fooDir2 != fooDir {
		t.Fatal("expected volume path to be %s, got: %s", fooDir, fooDir2)
	}

	logDone("container REST API - can use path from normal volume as bind-mount to overwrite another volume")
}
