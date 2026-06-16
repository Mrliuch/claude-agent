package wechat

import (
	"regexp"
	"strings"

	"claude-agent/internal/protocol"
)

// readOnlyTools 默认自动放行的只读工具(无副作用)。
var readOnlyTools = map[string]bool{
	"Read":         true,
	"Grep":         true,
	"Glob":         true,
	"LS":           true,
	"NotebookRead": true,
	"TodoWrite":    true, // 仅维护本地待办,无外部副作用
}

// dangerousCmds 是会改变系统状态/破坏性的命令(黑名单)。命中即需用户确认。
// 采用黑名单而非白名单:巡检/查看类只读命令千变万化无法穷举,默认放行、只拦危险操作。
var dangerousCmds = map[string]bool{
	// 文件写/删/移动
	"rm": true, "rmdir": true, "mv": true, "cp": true, "dd": true, "shred": true,
	"truncate": true, "tee": true, "ln": true, "touch": true, "mkdir": true,
	"install": true, "rsync": true,
	// 权限/属主
	"chmod": true, "chown": true, "chgrp": true, "chattr": true, "setfacl": true,
	// 磁盘/分区/文件系统
	"mkfs": true, "fdisk": true, "parted": true, "mount": true, "umount": true,
	"swapon": true, "swapoff": true, "lvremove": true, "lvcreate": true,
	// 进程/电源
	"kill": true, "killall": true, "pkill": true, "reboot": true, "shutdown": true,
	"halt": true, "poweroff": true, "init": true, "telinit": true,
	// 服务
	"service": true, "rc-service": true,
	// 用户/认证
	"useradd": true, "userdel": true, "usermod": true, "groupadd": true,
	"groupdel": true, "passwd": true, "chpasswd": true, "visudo": true,
	// 网络配置/防火墙
	"iptables": true, "ip6tables": true, "nft": true, "ufw": true, "firewall-cmd": true,
	"modprobe": true, "insmod": true, "rmmod": true,
	// 包管理
	"apt": true, "apt-get": true, "yum": true, "dnf": true, "rpm": true, "dpkg": true,
	"pip": true, "pip3": true, "npm": true, "yarn": true, "gem": true, "make": true,
	"helm": true,
	// docker/podman/git/kubectl 不整体拉黑,按子命令细分(见 bashSafe)
	// 任意代码执行 / 外联
	"eval": true, "exec": true, "source": true, "python": true, "python3": true,
	"perl": true, "ruby": true, "php": true, "node": true, "bash": true, "sh": true,
	"zsh": true, "nc": true, "ncat": true, "ssh": true, "scp": true, "curl": true,
	"wget": true, "crontab": true, "at": true,
}

// 双用途命令的只读子命令白名单(命中即放行,其余子命令需确认)。
var dockerReadOnlySub = map[string]bool{
	"ps": true, "images": true, "stats": true, "logs": true, "inspect": true,
	"version": true, "info": true, "top": true, "port": true, "events": true,
	"history": true, "df": true, "search": true,
}
var dockerNestedSub = map[string]bool{ // docker image/container/... 后再跟只读动词
	"image": true, "container": true, "volume": true, "network": true,
	"node": true, "service": true, "system": true, "compose": true, "context": true, "config": true,
}
var dockerNestedVerb = map[string]bool{"ls": true, "ps": true, "inspect": true, "logs": true, "df": true, "version": true, "view": true}

var gitReadOnlySub = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "branch": true,
	"remote": true, "rev-parse": true, "describe": true, "ls-files": true,
	"ls-remote": true, "blame": true, "tag": true, "config": true, "shortlog": true,
	"reflog": true, "cat-file": true, "name-rev": true, "grep": true, "for-each-ref": true, "whatchanged": true,
}
var kubectlReadOnlySub = map[string]bool{
	"get": true, "describe": true, "logs": true, "top": true, "version": true,
	"explain": true, "api-resources": true, "api-versions": true, "cluster-info": true, "config": true,
}

// 危险输出重定向:> / >> 指向非 /dev/null 的目标视为写操作。
var redirectRe = regexp.MustCompile(`(?:^|[^0-9&])>>?\s*([^\s;|&]+)`)

// quotedRe 匹配单/双引号包裹的字符串(用于剔除字符串字面量,避免其中的 > | && 误判)。
var quotedRe = regexp.MustCompile(`'[^']*'|"[^"]*"`)

// shellSplitRe 按 ; && || | 切分管道/命令序列。
var shellSplitRe = regexp.MustCompile(`\|\||&&|;|\|`)

// stripQuoted 把引号字符串替换为空格,消除字面量里的 shell 元字符干扰。
func stripQuoted(cmd string) string { return quotedRe.ReplaceAllString(cmd, " ") }

// autoApprove 判定一次权限请求是否可自动放行。
func autoApprove(toolName string, input any) bool {
	if readOnlyTools[toolName] {
		return true
	}
	if toolName == "Bash" {
		m, _ := input.(map[string]any)
		return bashSafe(protocol.StrOr(m["command"], ""))
	}
	return false // Write/Edit/未知工具 → 需确认
}

// bashSafe 判定一条 Bash 命令是否安全可自动放行(黑名单:无危险命令、无危险重定向)。
func bashSafe(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	cmd = stripQuoted(cmd) // 先剔除引号字面量,避免其中的元字符误判
	if hasDangerousRedirect(cmd) {
		return false
	}
	for _, seg := range shellSplitRe.Split(cmd, -1) {
		w := firstCommandWord(seg)
		if w == "" {
			continue // 空段(如重定向后的纯参数)
		}
		if dangerousCmds[w] {
			return false
		}
		// 双用途命令按子命令/参数细分
		switch w {
		case "systemctl":
			if systemctlMutates(seg) {
				return false
			}
		case "ip":
			if ipMutates(seg) {
				return false
			}
		case "sysctl":
			if strings.Contains(seg, "-w") || strings.Contains(seg, "=") {
				return false
			}
		case "find":
			if strings.Contains(seg, "-delete") || strings.Contains(seg, "-exec") {
				return false
			}
		case "sed":
			if sedInPlace(seg) {
				return false
			}
		case "awk", "gawk":
			if strings.Contains(seg, "system(") || strings.Contains(seg, "print >") {
				return false
			}
		case "docker", "podman":
			if !dockerSafe(seg) {
				return false
			}
		case "git":
			if !subFirstIn(seg, gitReadOnlySub) {
				return false
			}
		case "kubectl":
			if !subFirstIn(seg, kubectlReadOnlySub) {
				return false
			}
		}
	}
	return true
}

// subcommands 返回命令名之后的非选项参数序列(子命令链)。
func subcommands(seg string) []string {
	seg = strings.TrimSpace(seg)
	seg = strings.TrimLeft(seg, "({ \t")
	fields := strings.Fields(seg)
	var subs []string
	seenCmd := false
	for _, f := range fields {
		if !seenCmd {
			if f == "sudo" || (strings.Contains(f, "=") && !strings.HasPrefix(f, "-")) {
				continue
			}
			seenCmd = true // 这是命令名本身
			continue
		}
		if strings.HasPrefix(f, "-") {
			continue // 跳过选项
		}
		subs = append(subs, f)
	}
	return subs
}

// subFirstIn 判定第一个子命令是否在只读集合内。
func subFirstIn(seg string, set map[string]bool) bool {
	subs := subcommands(seg)
	return len(subs) > 0 && set[subs[0]]
}

// dockerSafe 判定 docker/podman 是否只读(含 image/container 等嵌套只读动词)。
func dockerSafe(seg string) bool {
	subs := subcommands(seg)
	if len(subs) == 0 {
		return false
	}
	if dockerReadOnlySub[subs[0]] {
		return true
	}
	if dockerNestedSub[subs[0]] && len(subs) > 1 && dockerNestedVerb[subs[1]] {
		return true
	}
	return false
}

// hasDangerousRedirect 检测写文件重定向(忽略 2>/dev/null、>/dev/null、2>&1 等)。
func hasDangerousRedirect(cmd string) bool {
	cmd = stripQuoted(cmd)
	for _, m := range redirectRe.FindAllStringSubmatch(cmd, -1) {
		if strings.TrimSpace(m[1]) != "/dev/null" {
			return true
		}
	}
	return false
}

// firstCommandWord 取一个管道段的命令名(跳过 sudo / 环境变量赋值 / 路径前缀)。
func firstCommandWord(seg string) string {
	seg = strings.TrimSpace(seg)
	seg = strings.TrimLeft(seg, "({ \t")
	for _, f := range strings.Fields(seg) {
		if f == "sudo" || f == "command" || f == "time" || f == "nice" || f == "exec" {
			// exec 单独在 dangerousCmds 中也会拦;此处跳过包裹词继续看真实命令
			if f == "exec" {
				return "exec"
			}
			continue
		}
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue // VAR=value 形式的前置赋值
		}
		if i := strings.LastIndex(f, "/"); i >= 0 {
			f = f[i+1:]
		}
		return f
	}
	return ""
}

// systemctlMutates 判定 systemctl 子命令是否改变状态。
func systemctlMutates(seg string) bool {
	mut := map[string]bool{
		"start": true, "stop": true, "restart": true, "reload": true,
		"enable": true, "disable": true, "mask": true, "unmask": true,
		"kill": true, "isolate": true, "set-property": true, "daemon-reload": true,
		"set-default": true, "edit": true, "reset-failed": true,
	}
	for _, f := range strings.Fields(seg) {
		if mut[f] {
			return true
		}
	}
	return false
}

// ipMutates 判定 ip 命令是否改网络配置(add/del/set/flush/change/replace)。
func ipMutates(seg string) bool {
	mut := map[string]bool{
		"add": true, "del": true, "delete": true, "set": true,
		"flush": true, "change": true, "replace": true,
	}
	for _, f := range strings.Fields(seg) {
		if mut[f] {
			return true
		}
	}
	return false
}

// sedInPlace 检测 sed 是否带原地写入标志(-i / -i.bak / --in-place)。
func sedInPlace(seg string) bool {
	for _, f := range strings.Fields(seg) {
		if f == "--in-place" || f == "-i" || strings.HasPrefix(f, "-i.") {
			return true
		}
	}
	return false
}
