// Copyright 2017-18 Daniel Swarbrick. All rights reserved.
// Use of this source code is governed by a GPL license that can be found in the LICENSE file.

// Config parsing.

package config

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

type InfluxDBConf struct {
	Url      string
	Database string
	Username string
	Password string
}

type FabricmonConf struct {
	BindAddress  string   `toml:"bind_address"`
	PollInterval Duration `toml:"poll_interval"`
	InfluxDB     []InfluxDBConf
}

// Duration is a TOML wrapper type for time.Duration.
// See https://github.com/golang/go/issues/24174
type Duration time.Duration

// String returns the string representation of the duration.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// UnmarshalText parses a TOML value into a duration value.
func (d *Duration) UnmarshalText(text []byte) error {
	// Ignore if there is no value set.
	if len(text) == 0 {
		return nil
	}

	// Otherwise parse as a duration formatted string.
	value, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}

	// Set duration and return.
	*d = Duration(value)
	return nil
}

// MarshalText converts a duration to a string for decoding TOML.
func (d Duration) MarshalText() (text []byte, err error) {
	return []byte(d.String()), nil
}

func ReadConfig(configFile string) (FabricmonConf, error) {
	// Defaults
	conf := FabricmonConf{
		BindAddress:  ":8090",
		PollInterval: Duration(time.Second * 10),
	}

	if _, err := toml.DecodeFile(configFile, &conf); err != nil {
		return conf, fmt.Errorf("Cannot open / parse config file: %s", err)
	}

	return conf, nil
}
