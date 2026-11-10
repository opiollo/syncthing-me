// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package main

import (
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/syncthing"
)

func startTray(app *syncthing.App, cfg config.Wrapper) func() {
	return func() {}
}
