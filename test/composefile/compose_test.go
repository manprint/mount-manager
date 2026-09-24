// Package composefile_test verifies the shape of the repository compose.yml.
package composefile_test

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func loadCompose(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../compose.yml")
	if err != nil {
		t.Fatalf("read compose.yml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal compose.yml: %v", err)
	}
	return doc
}

func services(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	svc, ok := doc["services"].(map[string]any)
	if !ok {
		t.Fatalf("services: not a map: %T", doc["services"])
	}
	return svc
}

func asMap(t *testing.T, v any, path string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: not a map: %T", path, v)
	}
	return m
}

func asSlice(t *testing.T, v any, path string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: not a slice: %T", path, v)
	}
	return s
}

func TestComposeBootstrap(t *testing.T) {
	doc := loadCompose(t)
	svc := services(t, doc)

	rclone := asMap(t, svc["rclone-mount"], "services.rclone-mount")
	healthcheck := asMap(t, rclone["healthcheck"], "services.rclone-mount.healthcheck")

	test := asSlice(t, healthcheck["test"], "services.rclone-mount.healthcheck.test")
	if len(test) != 2 {
		t.Fatalf("healthcheck.test length = %d, want 2", len(test))
	}
	if test[0] != "CMD-SHELL" {
		t.Fatalf("healthcheck.test[0] = %v, want CMD-SHELL", test[0])
	}
	cmd, ok := test[1].(string)
	if !ok || !strings.Contains(cmd, "fuse.rclone") || !strings.Contains(cmd, "/mnt/mount") {
		t.Fatalf("healthcheck.test[1] = %v, want to contain fuse.rclone and /mnt/mount", test[1])
	}

	startInterval, ok := healthcheck["start_interval"].(string)
	if !ok || startInterval != "2s" {
		t.Fatalf("healthcheck.start_interval = %v, want 2s", healthcheck["start_interval"])
	}

	superdufs := asMap(t, svc["superdufs"], "services.superdufs")
	dependsOn := asMap(t, superdufs["depends_on"], "services.superdufs.depends_on")
	rcloneDep := asMap(t, dependsOn["rclone-mount"], "services.superdufs.depends_on.rclone-mount")

	condition, ok := rcloneDep["condition"].(string)
	if !ok || condition != "service_healthy" {
		t.Fatalf("depends_on.rclone-mount.condition = %v, want service_healthy", rcloneDep["condition"])
	}
	restart, ok := rcloneDep["restart"].(bool)
	if !ok || restart != true {
		t.Fatalf("depends_on.rclone-mount.restart = %v, want true", rcloneDep["restart"])
	}
}

func TestComposeUnchangedFields(t *testing.T) {
	doc := loadCompose(t)

	name, ok := doc["name"].(string)
	if !ok || name != "rclone-stack-superdufs" {
		t.Fatalf("name = %v, want rclone-stack-superdufs", doc["name"])
	}

	svc := services(t, doc)
	rclone := asMap(t, svc["rclone-mount"], "services.rclone-mount")
	superdufs := asMap(t, svc["superdufs"], "services.superdufs")

	restart := rclone["restart"]
	restartStr := ""
	switch v := restart.(type) {
	case string:
		restartStr = v
	case bool:
		if v {
			restartStr = "true"
		} else {
			restartStr = "false"
		}
	default:
		t.Fatalf("rclone-mount.restart: unexpected type %T (%v)", restart, restart)
	}
	if restartStr != "no" && restartStr != "false" {
		t.Fatalf("rclone-mount.restart = %q, want one of {no, false}", restartStr)
	}

	for _, svcName := range []string{"rclone-mount", "superdufs"} {
		s := asMap(t, svc[svcName], "services."+svcName)
		pullPolicy, ok := s["pull_policy"].(string)
		if !ok || pullPolicy != "always" {
			t.Fatalf("services.%s.pull_policy = %v, want always", svcName, s["pull_policy"])
		}
	}

	rcloneVolumes := asSlice(t, rclone["volumes"], "services.rclone-mount.volumes")
	wantVolumes := []string{"./mymount:/mnt/mount:rshared", "./config:/config:rw"}
	for _, want := range wantVolumes {
		found := false
		for _, v := range rcloneVolumes {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("services.rclone-mount.volumes = %v, want to contain %q", rcloneVolumes, want)
		}
	}

	superdufsVolumes := asSlice(t, superdufs["volumes"], "services.superdufs.volumes")
	if len(superdufsVolumes) != 1 || superdufsVolumes[0] != "./mymount:/data" {
		t.Fatalf("services.superdufs.volumes = %v, want [./mymount:/data]", superdufsVolumes)
	}

	rcloneCommand := asSlice(t, rclone["command"], "services.rclone-mount.command")
	if len(rcloneCommand) != 30 {
		t.Fatalf("services.rclone-mount.command length = %d, want 30", len(rcloneCommand))
	}
	wantFirst := []string{"--config=/config/rclone.conf", "mount", "remote:/", "/mnt/mount"}
	for i, want := range wantFirst {
		if rcloneCommand[i] != want {
			t.Fatalf("services.rclone-mount.command[%d] = %v, want %q", i, rcloneCommand[i], want)
		}
	}

	image, ok := superdufs["image"].(string)
	if !ok || image != "fabiop85/superdufs:v1.5.0" {
		t.Fatalf("services.superdufs.image = %v, want fabiop85/superdufs:v1.5.0", superdufs["image"])
	}

	ports := asSlice(t, superdufs["ports"], "services.superdufs.ports")
	if len(ports) != 1 || ports[0] != "5000:5000" {
		t.Fatalf("services.superdufs.ports = %v, want [5000:5000]", ports)
	}
}
