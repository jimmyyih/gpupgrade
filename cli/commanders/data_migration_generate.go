// Copyright (c) 2017-2023 VMware, Inc. or its affiliates
// SPDX-License-Identifier: Apache-2.0

package commanders

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/xerrors"

	"github.com/greenplum-db/gpupgrade/greenplum"
	"github.com/greenplum-db/gpupgrade/greenplum/connection"
	"github.com/greenplum-db/gpupgrade/idl"
	"github.com/greenplum-db/gpupgrade/step"
	"github.com/greenplum-db/gpupgrade/upgrade"
	"github.com/greenplum-db/gpupgrade/utils"
	"github.com/greenplum-db/gpupgrade/utils/errorlist"
)

func GenerateDataMigrationScripts(nonInteractive bool, gphome string, port int, seedDir string, outputDir string, outputDirFS fs.FS) error {
	version, err := greenplum.Version(gphome)
	if err != nil {
		return err
	}

	switch {
	case version.Major == 5:
		seedDir = filepath.Join(seedDir, "5-to-6-seed-scripts")
	case version.Major == 6:
		seedDir = filepath.Join(seedDir, "6-to-7-seed-scripts")
	//case version.Major == 7:
	//	seedDir = filepath.Join(seedDir, "7-to-8-seed-scripts") // Need to add 7-8 seed scripts for 7-to-7 jobs.
	default:
		return fmt.Errorf("failed to find seed scripts for Greenplum version %s under %q", version, seedDir)
	}

	db, err := bootstrapConnectionFunc(idl.ClusterDestination_source, gphome, port)
	if err != nil {
		return err
	}
	defer func() {
		if cErr := db.Close(); cErr != nil {
			err = errorlist.Append(err, cErr)
		}
	}()

	err = utils.System.MkdirAll(outputDir, 0700)
	if err != nil {
		return err
	}

	err = ArchiveDataMigrationScriptsPrompt(nonInteractive, bufio.NewReader(os.Stdin), outputDirFS, outputDir)
	if err != nil {
		if errors.Is(err, step.Skip) {
			return nil
		}

		return err
	}

	databases, err := GetDatabases(db, utils.System.DirFS(seedDir))
	if err != nil {
		return err
	}

	fmt.Printf("\nGenerating data migration scripts for %d databases...\n", len(databases))
	progressBar := mpb.New()
	var wg sync.WaitGroup
	errChan := make(chan error, len(databases))

	for _, database := range databases {
		wg.Add(1)
		bar := progressBar.New(int64(database.NumSeedScripts),
			mpb.NopStyle(),
			mpb.PrependDecorators(decor.Name("  "+database.Datname, decor.WCSyncSpaceR)),
			mpb.AppendDecorators(decor.NewPercentage("%d")))

		go func(database DatabaseInfo, gphome string, port int, seedDir string, outputDir string, bar *mpb.Bar) {
			defer wg.Done()

			err = GenerateScriptsPerDatabase(database, gphome, port, seedDir, outputDir, bar)
			if err != nil {
				errChan <- err
				bar.Abort(false)
				return
			}

		}(database, gphome, port, seedDir, outputDir, bar)
	}

	progressBar.Wait()
	wg.Wait()
	close(errChan)

	var errs error
	for e := range errChan {
		errs = errorlist.Append(errs, e)
	}

	if errs != nil {
		return errs
	}

	logDir, err := utils.GetLogDir()
	if err != nil {
		return err
	}

	fmt.Printf(`
Generated scripts:
%s

Logs:
%s

`, utils.Bold.Sprint(filepath.Join(outputDir, "current")), utils.Bold.Sprint(logDir))

	return nil
}

var bootstrapConnectionFunc = connection.Bootstrap

// XXX: for internal testing only
func SetBootstrapConnectionFunction(connectionFunc func(destination idl.ClusterDestination, gphome string, port int) (*sql.DB, error)) {
	bootstrapConnectionFunc = connectionFunc
}

// XXX: for internal testing only
func ResetBootstrapConnectionFunction() {
	bootstrapConnectionFunc = connection.Bootstrap
}

func ArchiveDataMigrationScriptsPrompt(nonInteractive bool, reader *bufio.Reader, outputDirFS fs.FS, outputDir string) error {
	outputDirEntries, err := utils.System.ReadDirFS(outputDirFS, ".")
	if err != nil {
		return err
	}

	currentDir := filepath.Join(outputDir, "current")
	currentDirExists := false
	var currentDirModTime time.Time
	for _, entry := range outputDirEntries {
		if entry.IsDir() && entry.Name() == "current" {
			currentDirExists = true
			info, err := entry.Info()
			if err != nil {
				return err
			}

			currentDirModTime = info.ModTime()
		}
	}

	if !currentDirExists {
		return nil
	}

	for {
		fmt.Printf(`Previously generated data migration scripts found from
%s located in
%s

Archive and re-generate the data migration scripts if potentially 
new problematic objects have been added since the scripts were 
first generated. If unsure its safe to archive and re-generate 
the scripts.

The generator takes a "snapshot" of the current source cluster
to generate the scripts. If new "problematic" objects are added 
after the generator was run, then the previously generated 
scripts are outdated. The generator will need to be re-run 
to detect the newly added objects.`, currentDirModTime.Format(time.RFC1123Z), utils.Bold.Sprint(currentDir))

		input := "a"
		if !nonInteractive {
			fmt.Println()
			fmt.Printf(`
  [a]rchive and re-generate scripts
  [c]ontinue using previously generated scripts
  [q]uit

Select: `)
			rawInput, err := reader.ReadString('\n')
			if err != nil {
				return err
			}

			input = strings.ToLower(strings.TrimSpace(rawInput))
		}

		switch input {
		case "a":
			archiveDir := filepath.Join(outputDir, "archive", currentDirModTime.Format("20060102T1504"))
			exist, err := upgrade.PathExist(archiveDir)
			if err != nil {
				return err
			}

			if exist {
				log.Printf("Skip archiving data migration scripts as it already exists in %s\n", utils.Bold.Sprint(archiveDir))
				return step.Skip
			}

			fmt.Printf("\nArchiving previously generated scripts under\n%s\n", utils.Bold.Sprint(archiveDir))
			err = utils.System.MkdirAll(filepath.Dir(archiveDir), 0700)
			if err != nil {
				return fmt.Errorf("make directory: %w", err)
			}

			err = utils.Move(currentDir, archiveDir)
			if err != nil {
				return fmt.Errorf("move directory: %w", err)
			}

			return nil
		case "c":
			fmt.Printf("\nContinuing with previously generated data migration scripts in\n%s\n", utils.Bold.Sprint(currentDir))
			return step.Skip
		case "q":
			fmt.Print("\nQuitting...")
			return step.Quit
		default:
			continue
		}
	}
}

func GenerateScriptsPerDatabase(database DatabaseInfo, gphome string, port int, seedDir string, outputDir string, bar *mpb.Bar) error {
	output, err := executeSQLCommand(gphome, port, database.Datname, `CREATE LANGUAGE plpythonu;`)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}

	log.Println(string(output))

	// Create a schema to use while generating the scripts. However, the generated scripts cannot depend on this
	// schema as its dropped at the end of the generation process. If necessary, the generated scripts can use their
	// own temporary schema.
	output, err = executeSQLCommand(gphome, port, database.Datname, `DROP SCHEMA IF EXISTS __gpupgrade_tmp_generator CASCADE; CREATE SCHEMA __gpupgrade_tmp_generator;`)
	if err != nil {
		return err
	}

	log.Println(string(output))

	output, err = applySQLFile(gphome, port, database.Datname, filepath.Join(seedDir, "create_find_view_dep_function.sql"))
	if err != nil {
		return err
	}

	log.Println(string(output))

	var wg sync.WaitGroup
	errChan := make(chan error, len(MigrationScriptPhases))

	for _, phase := range MigrationScriptPhases {
		wg.Add(1)
		log.Printf("  Generating %q scripts for %s\n", phase, database.Datname)

		go func(phase idl.Step, database DatabaseInfo, gphome string, port int, seedDir string, outputDir string, bar *mpb.Bar) {
			defer wg.Done()

			err = GenerateScriptsPerPhase(phase, database, gphome, port, seedDir, utils.System.DirFS(seedDir), outputDir, bar)
			if err != nil {
				errChan <- err
				return
			}
		}(phase, database, gphome, port, seedDir, outputDir, bar)
	}

	wg.Wait()
	close(errChan)

	var errs error
	for e := range errChan {
		errs = errorlist.Append(errs, e)
	}

	if errs != nil {
		return errs
	}

	output, err = executeSQLCommand(gphome, port, database.Datname, `DROP TABLE IF EXISTS __gpupgrade_tmp_generator.__temp_views_list; DROP SCHEMA IF EXISTS __gpupgrade_tmp_generator CASCADE;`)
	if err != nil {
		return err
	}

	log.Println(string(output))
	return nil
}

func isGlobalScript(script string, database string) bool {
	// Generate one global script for the postgres database rather than all databases.
	return database != "postgres" && (script == "gen_alter_gphdfs_roles.sql" || script == "generate_cluster_stats.sh")
}

func GenerateScriptsPerPhase(phase idl.Step, database DatabaseInfo, gphome string, port int, seedDir string, seedDirFS fs.FS, outputDir string, bar *mpb.Bar) error {
	scriptDirs, err := fs.ReadDir(seedDirFS, phase.String())
	if err != nil {
		return err
	}

	if len(scriptDirs) == 0 {
		return xerrors.Errorf("Failed to generate data migration script. No seed files found in %q.", seedDir)
	}

	for _, scriptDir := range scriptDirs {
		scripts, err := utils.System.ReadDirFS(seedDirFS, filepath.Join(phase.String(), scriptDir.Name()))
		if err != nil {
			return err
		}

		for _, script := range scripts {
			if isGlobalScript(script.Name(), database.Datname) {
				continue
			}

			var scriptOutput []byte
			if strings.HasSuffix(script.Name(), ".sql") {
				scriptOutput, err = applySQLFile(gphome, port, database.Datname, filepath.Join(seedDir, phase.String(), scriptDir.Name(), script.Name()),
					"-v", "ON_ERROR_STOP=1", "--no-align", "--tuples-only")
				if err != nil {
					return err
				}
			}

			if strings.HasSuffix(script.Name(), ".sh") || strings.HasSuffix(script.Name(), ".bash") {
				scriptOutput, err = executeBashFile(gphome, port, filepath.Join(seedDir, phase.String(), scriptDir.Name(), script.Name()), database.Datname)
				if err != nil {
					return err
				}
			}

			if len(scriptOutput) == 0 {
				// Increment bar even when there is no generated script written since the bar is tied to seed scripts executed rather than written.
				bar.Increment()
				continue
			}

			var contents bytes.Buffer
			contents.WriteString(`\c ` + database.QuotedDatname + "\n")

			headerOutput, err := utils.System.ReadFileFS(seedDirFS, filepath.Join(phase.String(), scriptDir.Name(), strings.TrimSuffix(script.Name(), path.Ext(script.Name()))+".header"))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}

			contents.Write(headerOutput)
			contents.Write(scriptOutput)

			outputPath := filepath.Join(outputDir, "current", phase.String(), scriptDir.Name())
			err = utils.System.MkdirAll(outputPath, 0700)
			if err != nil {
				return err
			}

			outputFile := "migration_" + database.QuotedDatname + "_" + strings.TrimSuffix(script.Name(), filepath.Ext(script.Name())) + ".sql"
			err = utils.System.WriteFile(filepath.Join(outputPath, outputFile), contents.Bytes(), 0644)
			if err != nil {
				return err
			}

			bar.Increment()
		}
	}

	return nil
}

type DatabaseInfo struct {
	Datname        string
	QuotedDatname  string
	NumSeedScripts int
}

func GetDatabases(db *sql.DB, seedDirFS fs.FS) ([]DatabaseInfo, error) {
	rows, err := db.Query(`SELECT datname, quote_ident(datname) AS quoted_datname FROM pg_database WHERE datname != 'template0';`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []DatabaseInfo
	for rows.Next() {
		var database DatabaseInfo
		err = rows.Scan(&database.Datname, &database.QuotedDatname)
		if err != nil {
			return nil, xerrors.Errorf("pg_database: %w", err)
		}

		numSeedScripts, err := countSeedScripts(database.Datname, seedDirFS)
		if err != nil {
			return nil, err
		}

		database.NumSeedScripts = numSeedScripts

		databases = append(databases, database)
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return databases, nil
}

func countSeedScripts(database string, seedDirFS fs.FS) (int, error) {
	var numSeedScripts int

	phasesEntries, err := utils.System.ReadDirFS(seedDirFS, ".")
	if err != nil {
		return 0, err
	}

	for _, phaseEntry := range phasesEntries {
		if !phaseEntry.IsDir() || !isPhase(phaseEntry.Name()) {
			continue
		}

		seedScriptDirs, err := fs.ReadDir(seedDirFS, phaseEntry.Name())
		if err != nil {
			return 0, err
		}

		for _, seedScriptDir := range seedScriptDirs {
			seedScripts, err := utils.System.ReadDirFS(seedDirFS, filepath.Join(phaseEntry.Name(), seedScriptDir.Name()))
			if err != nil {
				return 0, err
			}

			for _, seedScript := range seedScripts {
				if isGlobalScript(seedScript.Name(), database) {
					continue
				}

				numSeedScripts += 1
			}
		}
	}

	return numSeedScripts, nil
}

func isPhase(input string) bool {
	for _, phase := range MigrationScriptPhases {
		if input == phase.String() {
			return true
		}
	}

	return false
}
