/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package contract

import (
	"os"
	"strings"
	"testing"
)

// TestGoldenInspectBootCmdline_MatchesProvisioningRender keeps the inspect-mode
// fixture on the provisioning fixture's inputs until the controller renders
// inspect boots (#289); from then on the real render guards it instead.
func TestGoldenInspectBootCmdline_MatchesProvisioningRender(t *testing.T) {
	provisioning, err := os.ReadFile("golden_boot_cmdline.txt")
	if err != nil {
		t.Fatalf("read golden_boot_cmdline.txt: %v", err)
	}
	inspect, err := os.ReadFile("golden_inspect_boot_cmdline.txt")
	if err != nil {
		t.Fatalf("read golden_inspect_boot_cmdline.txt: %v", err)
	}

	claimOnly := map[string]bool{
		"beskar7.target":        true,
		"beskar7.target-digest": true,
		"beskar7.provider-id":   true,
		"beskar7.disk":          true,
		"beskar7.ip":            true,
	}
	lines := strings.Split(string(provisioning), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "kernel ") {
			continue
		}
		var kept []string
		for _, field := range strings.Split(line, " ") {
			key, _, _ := strings.Cut(field, "=")
			if claimOnly[key] {
				continue
			}
			kept = append(kept, field)
			if key == "beskar7.token" {
				kept = append(kept, "beskar7.mode=inspect")
			}
		}
		lines[i] = strings.Join(kept, " ")
	}

	if want := strings.Join(lines, "\n"); string(inspect) != want {
		t.Errorf("golden_inspect_boot_cmdline.txt is not the provisioning render without a claim's "+
			"parameters and with beskar7.mode=inspect after beskar7.token (contract v4.5 §4.1)\n got: %q\nwant: %q",
			inspect, want)
	}
}
