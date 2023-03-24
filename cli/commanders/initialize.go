// Copyright (c) 2017-2023 VMware, Inc. or its affiliates
// SPDX-License-Identifier: Apache-2.0

package commanders

import (
	"fmt"
	"log"
	"os"
	"os/exec"

	"golang.org/x/xerrors"

	"github.com/greenplum-db/gpupgrade/step"
	"github.com/greenplum-db/gpupgrade/upgrade"
	"github.com/greenplum-db/gpupgrade/utils"
)

var execCommandHubStart = exec.Command
var execCommandHubCount = exec.Command

// CreateStateDir creates the state directory in the cli to ensure that at most
// one gpupgrade is occurring at the same time.
func CreateStateDir() (err error) {
	stateDir := utils.GetStateDir()

	err = os.Mkdir(stateDir, 0700)
	if os.IsExist(err) {
		log.Printf("State directory %s already present. Skipping.", stateDir)
		return nil
	}

	if err != nil {
		return xerrors.Errorf("creating state directory %q: %w", stateDir, err)
	}

	return nil
}

func CreateTemporaryConfigFile(gphome string, port int) error {
	path := upgrade.GetConfigFile() + ".tmp"

	exist, err := upgrade.PathExist(path)
	if err != nil {
		return xerrors.Errorf("checking temporary configuration path %q: %w", path, err)
	}

	if exist {
		log.Printf("Temporary configuration file %s already present. Skipping.", path)
		return nil
	}

	err = os.WriteFile(path, []byte(fmt.Sprintf(`{"GPHome": %s, "Port": %d}`, gphome, port)), 0644)
	if err != nil {
		return err
	}

	return nil
}

func RemoveTemporaryConfigFile() error {
	path := upgrade.GetConfigFile() + ".tmp"
	return utils.Remove(path)
}

func CreateConfigFile(hubPort int) error {
	path := upgrade.GetConfigFile()

	exist, err := upgrade.PathExist(path)
	if err != nil {
		return xerrors.Errorf("checking configuration path %q: %w", path, err)
	}

	if exist {
		log.Printf("Configuration file %s already present. Skipping.", path)
		return nil
	}

	// Bootstrap with the port to enable the CLI helper function connectToHub to
	// work with both initialize and all other CLI commands. This overloads the
	// hub's persisted configuration with that of the CLI when ideally these
	// would be separate.
	err = os.WriteFile(path, []byte(fmt.Sprintf(`{"Port": %d}`, hubPort)), 0644)
	if err != nil {
		return err
	}

	return nil
}

func StartHub() (err error) {
	running, err := IsHubRunning()
	if err != nil {
		return xerrors.Errorf("is hub running: %w", err)
	}

	if running {
		log.Printf("Hub already running. Skipping.")
		return step.Skip
	}

	cmd := execCommandHubStart("gpupgrade", "hub", "--daemonize")
	log.Printf("Executing: %q", cmd.String())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return xerrors.Errorf("%q failed with %q: %w", cmd.String(), string(output), err)
	}

	log.Printf("%s", output)
	return nil
}

func IsHubRunning() (bool, error) {
	script := `ps -ef | grep -wGc "[g]pupgrade hub"` // use square brackets to avoid finding yourself in matches
	_, err := execCommandHubCount("bash", "-c", script).Output()

	if exitError, ok := err.(*exec.ExitError); ok {
		if exitError.ProcessState.ExitCode() == 1 { // hub not found
			return false, nil
		}
	}
	if err != nil { // grep failed
		return false, err
	}

	return true, nil
}
