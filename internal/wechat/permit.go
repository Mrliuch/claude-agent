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

// readOnlyBashCmds 判定为只读的 Bash 命令(巡检/查看类)。
var readOnlyBashCmds = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "less": true, "more": true,
	"tac": true, "nl": true, "echo": true, "printf": true, "pwd": true, "whoami": true,
	"id": true, "date": true, "uptime": true, "cal": true,
	"df": true, "du": true, "free": true, "ps": true, "top": true, "vmstat": true,
	"iostat": true, "mpstat": true, "sar": true, "uname": true, "hostname": true,
	"arch": true, "nproc": true, "lscpu": true, "lsblk": true, "lsof": true,
	"ip": true, "ifconfig": true, "route": true, "ss": true, "netstat": true,
	"ping": true, "dig": true, "nslookup": true, "getent": true, "host": true,
	"grep": true, "egrep": true, "fgrep": true, "awk": true, "wc": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "column": true, "stat": true, "file": true,
	"which": true, "type": true, "env": true, "printenv": true, "who": true, "w": true,
	"last": true, "dmesg": true, "journalctl": true, "timedatectl": true,
	"basename": true, "dirname": true, "readlink": true, "realpath": true,
	"md5sum": true, "sha256sum": true, "true": true, "test": true, "find": true, "sed": true,
}

// 危险输出重定向:> / >> 指向非 /dev/null 的目标视为写操作。
var redirectRe = regexp.MustCompile(`(?:^|[^0-9&])>>?\s*([^\s;|&]+)`)

// quotedRe 匹配单/双引号包裹的字符串(用于剔除字符串字面量,避免其中的 > | && 误判)。
var quotedRe = regexp.MustCompile(`'[^']*'|"[^"]*"`)

// stripQuoted 把引号字符串替换为空格,消除字面量里的 shell 元字符干扰。
func stripQuoted(cmd string) string { return quotedRe.ReplaceAllString(cmd, " ") }

// shellSplitRe 按 ; && || | 切分管道/命令序列(|| && 优先于单 |)。
var shellSplitRe = regexp.MustCompile(`\|\||&&|;|\|`)

// autoApprove 判定一次权限请求是否可自动放行(只读)。
func autoApprove(toolName string, input any) bool {
	if readOnlyTools[toolName] {
		return true
	}
	if toolName == "Bash" {
		m, _ := input.(map[string]any)
		return bashReadOnly(protocol.StrOr(m["command"], ""))
	}
	return false
}

// bashReadOnly 判定一条 Bash 命令是否纯只读(全部命令在白名单且无危险重定向)。
func bashReadOnly(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	// 先剔除引号字符串字面量,避免其中的 > | && ; 等被误判。
	cmd = stripQuoted(cmd)
	if hasDangerousRedirect(cmd) {
		return false
	}
	for _, seg := range shellSplitRe.Split(cmd, -1) {
		w := firstCommandWord(seg)
		if w == "" || !readOnlyBashCmds[w] {
			return false
		}
		if w == "find" && (strings.Contains(seg, "-delete") || strings.Contains(seg, "-exec")) {
			return false
		}
		if w == "sed" && sedInPlace(seg) {
			return false
		}
	}
	return true
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
		if f == "sudo" {
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

// sedInPlace 检测 sed 是否带原地写入标志(-i / -i.bak / --in-place)。
func sedInPlace(seg string) bool {
	for _, f := range strings.Fields(seg) {
		if f == "--in-place" || f == "-i" || strings.HasPrefix(f, "-i.") {
			return true
		}
	}
	return false
}
