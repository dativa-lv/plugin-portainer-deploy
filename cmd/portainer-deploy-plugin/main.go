// Copyright (c) 2020, the Drone Plugins project authors.
// Copyright (c) 2021, Robert Kaussow <mail@thegeeklab.de>

// Use of this source code is governed by an Apache 2.0 license that can be
// found in the LICENSE file.

package main

import (
	plugin "github.com/dativa-lv/plugin-portainer-deploy"
)

var Version = "unknown"

func main() {
	plugin.New(Version).Run()
}
