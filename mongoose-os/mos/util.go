package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cesanta/errors"
	"github.com/golang/glog"
)

func reportf(f string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	glog.Infof(f, args...)
}

func prompt(text string) string {
	fmt.Fprintf(os.Stderr, "%s ", text)
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(ans)
}

func getCommandOutput(command string, args ...string) (string, error) {
	glog.Infof("Running %s %s", command, args)
	cmd := exec.Command(command, args...)
	output, err := cmd.Output()
	if err != nil {
		return "", errors.Annotatef(err, "failed to run %s %s", command, args)
	}
	return string(output), nil
}

// If some command causes the device to reboot, the reboot actually happens
// after 100ms, so that the device is able to respond to the RPC request
// which causes the reboot.
//
// We shouldn't issue the next RPC request until the reboot happens, so
// waitForReboot should be called after each request which causes the reboot.
func waitForReboot() {
	time.Sleep(200 * time.Millisecond)
}
