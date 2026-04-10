// SPDX-License-Identifier: Apache-2.0

// Package main provides the portainer-deploy-plugin CLI entrypoint.
package main

import (
	plugin "github.com/dativa-lv/plugin-portainer-deploy"
)

var Version = "develop"

func main() {
	plugin.New(Version).Run()
}
