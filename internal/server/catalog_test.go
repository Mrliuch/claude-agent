package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanSkillsReadsDescription(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inspect", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: inspect\ndescription: 检查主机基础状态\n---\n# Inspect\n"), 0644); err != nil {
		t.Fatal(err)
	}
	items := scanSkills(dir, "系统")
	if len(items) != 1 || items[0]["description"] != "检查主机基础状态" {
		t.Fatalf("unexpected skills: %#v", items)
	}
}
