// Copyright (c) 2017-2023 VMware, Inc. or its affiliates
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/greenplum-db/gpupgrade/cli/commanders"
	"github.com/greenplum-db/gpupgrade/idl"
	"github.com/greenplum-db/gpupgrade/step"
	"github.com/greenplum-db/gpupgrade/upgrade"
	"github.com/greenplum-db/gpupgrade/utils"
)

func revert() *cobra.Command {
	var verbose bool
	var nonInteractive bool

	cmd := &cobra.Command{
		Use:   "revert",
		Short: "reverts the upgrade and returns the cluster to its original state",
		Long:  RevertHelp,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			var response idl.RevertResponse
			var sourceGPHome string
			var sourcePort int

			logdir, err := utils.GetLogDir()
			if err != nil {
				return err
			}

			confirmationText := fmt.Sprintf(revertConfirmationText, logdir)

			st, err := commanders.NewStep(idl.Step_revert,
				&step.BufferedStreams{},
				verbose,
				nonInteractive,
				confirmationText,
			)
			if err != nil {
				if errors.Is(err, step.UserCanceled) {
					// If user cancels don't return an error to main to avoid
					// printing "Error:".
					return nil
				}
				return err
			}

			path := upgrade.GetConfigFile() + ".tmp"
			exist, err := upgrade.PathExist(path)
			if err != nil {
				return xerrors.Errorf("checking temporary configuration path %q: %w", path, err)
			}
			if exist {
				tempConfig := new(commands.TemporaryConfig)
				commands.LoadConfig(tempConfig, path)
				sourceGPHome = tempConfig.GPHome
				sourcePort = tempConfig.Port

				st.RunCLISubstep(idl.Substep_archive_log_directories, func(streams step.OutStreams) error {
					// Removing the state directory removes the step status file.
					// Disable the store so the step framework does not try to write
					// to a non-existent status file.
					st.DisableStore()
					return upgrade.DeleteDirectories([]string{utils.GetStateDir()}, upgrade.StateDirectoryFiles, streams)
				})
			} else {
				st.RunHubSubstep(func(streams step.OutStreams) error {
					client, err := connectToHub()
					if err != nil {
						return err
					}

					response, err = commanders.Revert(client, verbose)
					if err != nil {
						return err
					}

					sourceGPHome = response.GetSource().GPHome
					sourcePort = int(response.GetSource().GetPort())

					return nil
				})

				st.RunCLISubstep(idl.Substep_stop_hub_and_agents, func(streams step.OutStreams) error {
					return stopHubAndAgents()
				})
			}

			st.RunCLISubstepConditionally(idl.Substep_execute_revert_data_migration_scripts, !nonInteractive, func(streams step.OutStreams) error {
				fmt.Println()
				fmt.Println()

				currentDir := filepath.Join(response.GetLogArchiveDirectory(), "data-migration-scripts", "current")
				return commanders.ApplyDataMigrationScripts(nonInteractive, sourceGPHome, sourcePort),
					utils.System.DirFS(currentDir), currentDir, idl.Step_revert)
			})

			st.RunCLISubstep(idl.Substep_delete_master_statedir, func(streams step.OutStreams) error {
				// Removing the state directory removes the step status file.
				// Disable the store so the step framework does not try to write
				// to a non-existent status file.
				st.DisableStore()
				return upgrade.DeleteDirectories([]string{utils.GetStateDir()}, upgrade.StateDirectoryFiles, streams)
			})

			return st.Complete(fmt.Sprintf(`
Revert completed successfully.

The source cluster is now running version %s.
source %s
export MASTER_DATA_DIRECTORY=%s
export PGPORT=%d

The gpupgrade logs can be found on the master and segment hosts in
%s

NEXT ACTIONS
------------
If you have not already, execute the “%s” data migration scripts with
"gpupgrade apply --gphome %s --port %d --input-dir %s --phase %s"

To restart the upgrade, run "gpupgrade initialize" again.`,
				response.GetSourceVersion(),
				filepath.Join(response.GetSource().GetGPHome(), "greenplum_path.sh"), response.GetSource().GetCoordinatorDataDirectory(), response.GetSource().GetPort(),
				response.GetLogArchiveDirectory(),
				idl.Step_revert,
				response.GetSource().GetGPHome(), response.GetSource().GetPort(), filepath.Join(response.GetLogArchiveDirectory(), "data-migration-scripts"), idl.Step_revert))
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "print the output stream from all substeps")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "do not prompt for confirmation to proceed")
	cmd.Flags().MarkHidden("non-interactive") //nolint

	return addHelpToCommand(cmd, RevertHelp)
}

func ArchiveCoordinatorLogDirectory(logArchiveDir string) error {
	// Archive log directory on coordinator
	logDir, err := utils.GetLogDir()
	if err != nil {
		return err
	}

	log.Printf("archiving log directory %q to %q", logDir, logArchiveDir)
	if err = utils.Move(logDir, logArchiveDir); err != nil {
		return err
	}
}
