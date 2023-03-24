// Copyright (c) 2017-2023 VMware, Inc. or its affiliates
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"golang.org/x/xerrors"
)

type TemporaryConfig struct {
	GPHome string
	Port   int32
}

func (c *TemporaryConfig) Load(r io.Reader) error {
	dec := json.NewDecoder(r)
	return dec.Decode(c)
}

func LoadConfig(conf *TemporaryConfig, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return xerrors.Errorf("opening temporary configuration file: %w", err)
	}
	defer file.Close()

	err = conf.Load(file)
	if err != nil {
		return xerrors.Errorf("reading temporary configuration file: %w", err)
	}

	return nil
}
