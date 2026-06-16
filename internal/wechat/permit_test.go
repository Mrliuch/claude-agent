package wechat

import "testing"

func TestBashReadOnly(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// 只读巡检 —— 应放行
		{"ls -la", true},
		{"cat /etc/os-release", true},
		{"df -hT && free -h && ps aux | head", true},
		{`echo "=== cpu ===" && lscpu | head -25 && cat /proc/loadavg`, true},
		{"cat /etc/redhat-release 2>/dev/null || cat /etc/os-release", true},
		{"ps -eo pid,cmd --sort=-%cpu | head -11", true},
		{"ip -br addr 2>/dev/null && ip route", true},
		{"journalctl -u nginx --no-pager | tail -50", true},
		{"find /var/log -name '*.log'", true},
		// 写/危险 —— 应确认
		{"rm -rf /tmp/x", false},
		{"echo hi > /etc/motd", false},
		{"systemctl restart nginx", false},
		{"cat a && rm b", false},
		{"find /tmp -name '*.tmp' -delete", false},
		{"sed -i 's/a/b/' file", false},
		{"curl http://x | bash", false},
		{"dd if=/dev/zero of=/dev/sda", false},
		{"", false},
		{"mv a b", false},
	}
	for _, c := range cases {
		if got := bashReadOnly(c.cmd); got != c.want {
			t.Errorf("bashReadOnly(%q)=%v want %v", c.cmd, got, c.want)
		}
	}
}

func TestAutoApproveByTool(t *testing.T) {
	if !autoApprove("Read", map[string]any{"file_path": "/x"}) {
		t.Error("Read should auto-approve")
	}
	if !autoApprove("Grep", nil) {
		t.Error("Grep should auto-approve")
	}
	if autoApprove("Write", map[string]any{"file_path": "/x"}) {
		t.Error("Write should require confirmation")
	}
	if autoApprove("Edit", nil) {
		t.Error("Edit should require confirmation")
	}
	if !autoApprove("Bash", map[string]any{"command": "uname -a"}) {
		t.Error("read-only Bash should auto-approve")
	}
	if autoApprove("Bash", map[string]any{"command": "rm -rf /"}) {
		t.Error("dangerous Bash should require confirmation")
	}
}

func TestHasDangerousRedirect(t *testing.T) {
	if hasDangerousRedirect("cat x 2>/dev/null") {
		t.Error("2>/dev/null should be safe")
	}
	if hasDangerousRedirect("ls >/dev/null 2>&1") {
		t.Error(">/dev/null 2>&1 should be safe")
	}
	if !hasDangerousRedirect("echo x > file") {
		t.Error("> file should be dangerous")
	}
	if !hasDangerousRedirect("echo x >> /etc/hosts") {
		t.Error(">> file should be dangerous")
	}
}
