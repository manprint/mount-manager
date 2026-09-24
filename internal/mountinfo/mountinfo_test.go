package mountinfo

import (
	"strings"
	"testing"
)

func TestParseBasic(t *testing.T) {
	data := "" +
		"36 35 0:31 / /mnt/mount rw,noatime shared:1 master:2 - fuse.rclone remote: rw,user_id=0,group_id=0\n" +
		"25 30 8:1 / /boot rw,relatime - ext4 /dev/sda1 rw\n"

	entries, err := Parse(strings.NewReader(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	e0 := entries[0]
	if e0.MountID != 36 || e0.ParentID != 35 {
		t.Errorf("e0 MountID/ParentID = %d/%d, want 36/35", e0.MountID, e0.ParentID)
	}
	if e0.Major != 0 || e0.Minor != 31 {
		t.Errorf("e0 Major/Minor = %d/%d, want 0/31", e0.Major, e0.Minor)
	}
	if e0.Root != "/" || e0.MountPoint != "/mnt/mount" {
		t.Errorf("e0 Root/MountPoint = %q/%q", e0.Root, e0.MountPoint)
	}
	if e0.Options != "rw,noatime" {
		t.Errorf("e0 Options = %q", e0.Options)
	}
	if e0.FSType != "fuse.rclone" {
		t.Errorf("e0 FSType = %q, want fuse.rclone", e0.FSType)
	}
	if e0.Source != "remote:" {
		t.Errorf("e0 Source = %q, want remote:", e0.Source)
	}
	if e0.SuperOptions != "rw,user_id=0,group_id=0" {
		t.Errorf("e0 SuperOptions = %q", e0.SuperOptions)
	}

	e1 := entries[1]
	if e1.Major != 8 || e1.Minor != 1 {
		t.Errorf("e1 Major/Minor = %d/%d, want 8/1", e1.Major, e1.Minor)
	}
	if e1.FSType != "ext4" || e1.Source != "/dev/sda1" {
		t.Errorf("e1 FSType/Source = %q/%q", e1.FSType, e1.Source)
	}
	if e1.SuperOptions != "rw" {
		t.Errorf("e1 SuperOptions = %q, want rw", e1.SuperOptions)
	}
}

func TestParseEscapedMountPoint(t *testing.T) {
	data := "36 35 0:31 / /mnt/my\\040mount rw,noatime - fuse.rclone remote: rw\n"

	entries, err := Parse(strings.NewReader(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].MountPoint != "/mnt/my mount" {
		t.Errorf("MountPoint = %q, want %q", entries[0].MountPoint, "/mnt/my mount")
	}
}

func TestParseMalformed(t *testing.T) {
	data := "" +
		"36 35 0:31 / /mnt/mount rw,noatime - fuse.rclone remote: rw\n" +
		"25 30 8:1 / /boot rw,relatime ext4 /dev/sda1 rw\n"

	_, err := Parse(strings.NewReader(data))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "line 2")
	}
}

func TestTopReturnsLastStacked(t *testing.T) {
	entries := []Entry{
		{MountID: 1, MountPoint: "/mnt/mount"},
		{MountID: 2, MountPoint: "/mnt/mount"},
	}
	got, ok := Top(entries, "/mnt/mount")
	if !ok {
		t.Fatal("Top: not found")
	}
	if got.MountID != 2 {
		t.Errorf("Top MountID = %d, want 2 (last stacked)", got.MountID)
	}
}

func TestTopNotFound(t *testing.T) {
	entries := []Entry{
		{MountID: 1, MountPoint: "/mnt/other"},
	}
	_, ok := Top(entries, "/mnt/mount")
	if ok {
		t.Fatal("Top: expected not found")
	}
}

func TestReadSelf(t *testing.T) {
	entries, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("Read: got 0 entries")
	}
	found := false
	for _, e := range entries {
		if e.MountPoint == "/" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Read: no entry with MountPoint \"/\"")
	}
}
