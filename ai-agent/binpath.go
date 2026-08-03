package main

// Поиск бинарников CLI-инструментов (claude, codex).
//
// Зачем отдельный резолвер: агент работает как systemd user-сервис, а systemd
// даёт процессу минимальный PATH (/usr/local/bin:/usr/bin:/bin:...) — без
// ~/.local/bin и без nvm-каталогов. Логин-шелл эти пути добавляет, сервис —
// нет, поэтому exec.LookPath регулярно «теряет» установленный инструмент.
//
// Решение: если в PATH не нашли, обходим стандартные места установки
// (в т.ч. nvm/fnm/volta/nodenv — там берём самую свежую версию node).
// Плюс дочернему процессу подставляем PATH с каталогом найденного бинарника:
// codex — это JS-скрипт с `#!/usr/bin/env node`, и без node рядом он не
// запустится, даже когда путь до самого codex известен.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// envVar — переменная окружения с явным путём до инструмента (приоритет выше всего).
func envVar(tool string) string {
	return "AI_AGENT_" + strings.ToUpper(tool)
}

var (
	binMu    sync.Mutex
	binCache = map[string]string{}
)

// toolPath возвращает абсолютный путь до бинарника инструмента.
// Успешный результат кэшируется; неудача — нет, чтобы установку инструмента
// подхватило без перезапуска агента.
func toolPath(tool string) (string, error) {
	binMu.Lock()
	cached, ok := binCache[tool]
	binMu.Unlock()
	if ok && isExecutable(cached) {
		return cached, nil
	}

	if p, err := resolveTool(tool); err == nil {
		binMu.Lock()
		if binCache[tool] != p {
			log.Printf("%s resolved to %s", tool, p)
		}
		binCache[tool] = p
		binMu.Unlock()
		return p, nil
	} else {
		binMu.Lock()
		delete(binCache, tool)
		binMu.Unlock()
		return "", err
	}
}

// resolveTool — сам поиск, без кэша.
func resolveTool(tool string) (string, error) {
	// 1. явный путь из окружения (AI_AGENT_CLAUDE / AI_AGENT_CODEX)
	name := tool
	if v := strings.TrimSpace(os.Getenv(envVar(tool))); v != "" {
		name = v
	}
	if strings.ContainsRune(name, os.PathSeparator) {
		abs, err := filepath.Abs(name)
		if err == nil && isExecutable(abs) {
			return abs, nil
		}
		return "", fmt.Errorf("%s: %s — файл не найден или не исполняемый", envVar(tool), name)
	}
	tool = name

	// 2. PATH — как обычно
	if p, err := exec.LookPath(tool); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
		return p, nil
	}

	// 3. стандартные места установки
	home, _ := os.UserHomeDir()
	dirs := searchDirs(home)
	if p := findIn(dirs, tool); p != "" {
		return p, nil
	}

	return "", fmt.Errorf("%s не найден ни в PATH, ни в стандартных местах установки (%s); "+
		"укажите путь явно: %s=/полный/путь/до/%s",
		tool, strings.Join(shorten(home, dirs), ", "), envVar(tool), tool)
}

// searchDirs — каталоги, куда ставят node-CLI, в порядке приоритета.
// Каталоги менеджеров версий (nvm и ко.) раскрываются в конкретные версии,
// от новых к старым: v16 рядом с v20 — не редкость, и брать надо свежую.
func searchDirs(home string) []string {
	var dirs []string
	add := func(parts ...string) {
		if home == "" && !filepath.IsAbs(parts[0]) {
			return
		}
		dirs = append(dirs, filepath.Join(append([]string{home}, parts...)...))
	}

	// менеджеры версий node — сначала, там обычно самая свежая установка
	for _, g := range []string{
		filepath.Join(home, ".nvm", "versions", "node", "*", "bin"),
		filepath.Join(home, ".local", "share", "fnm", "node-versions", "*", "installation", "bin"),
		filepath.Join(home, ".fnm", "node-versions", "*", "installation", "bin"),
		filepath.Join(home, ".nodenv", "versions", "*", "bin"),
		filepath.Join(home, ".volta", "tools", "image", "node", "*", "bin"),
	} {
		if home == "" {
			continue
		}
		dirs = append(dirs, expandVersions(g)...)
	}

	add(".local", "bin")
	add("bin")
	add(".nvm", "current", "bin")
	add(".npm-global", "bin")
	add(".npm-packages", "bin")
	add(".node_modules", "bin")
	add(".yarn", "bin")
	add(".config", "yarn", "global", "node_modules", ".bin")
	add(".local", "share", "pnpm")
	add(".bun", "bin")
	add(".deno", "bin")
	add(".volta", "bin")
	add(".cargo", "bin")
	add(".asdf", "shims")

	dirs = append(dirs,
		"/usr/local/bin",
		"/opt/homebrew/bin",
		"/usr/bin",
		"/snap/bin",
	)
	return dirs
}

// expandVersions раскрывает glob с версиями и сортирует их по убыванию
// (v20.19.6 идёт раньше v16.20.2 — сравниваем числа, а не строки).
func expandVersions(pattern string) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil
	}
	// ключ сортировки — версия из компонента пути перед bin/installation
	key := func(p string) []int {
		parts := strings.Split(filepath.ToSlash(p), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if v := parseVersion(parts[i]); v != nil {
				return v
			}
		}
		return nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return compareVersions(key(matches[i]), key(matches[j])) > 0
	})
	return matches
}

// parseVersion разбирает "v20.19.6" / "20.19.6" в [20 19 6]; иначе nil.
func parseVersion(s string) []int {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ".") {
		// хвост вида "1-rc" отбрасываем, но цифры в начале берём
		i := 0
		for i < len(part) && part[i] >= '0' && part[i] <= '9' {
			i++
		}
		if i == 0 {
			return nil
		}
		n, err := strconv.Atoi(part[:i])
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// findIn — первый исполняемый файл с таким именем в списке каталогов.
func findIn(dirs []string, name string) string {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if isExecutable(p) {
			return p
		}
	}
	return ""
}

func isExecutable(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p) // Stat, а не Lstat: почти все установки — симлинки
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// shorten — пути для сообщения об ошибке: домашний каталог как ~,
// версии менеджеров сворачиваем обратно в glob, чтобы список не разъезжался.
func shorten(home string, dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		if home != "" && strings.HasPrefix(d, home+string(os.PathSeparator)) {
			d = "~" + strings.TrimPrefix(d, home)
		}
		if i := strings.Index(d, "/.nvm/versions/node/"); i >= 0 {
			d = d[:i] + "/.nvm/versions/node/*/bin"
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// toolEnv — окружение дочернего процесса: в начало PATH добавляем каталог
// найденного бинарника (и каталог цели, если это симлинк). Без этого
// nvm-установленный codex падает на своём `#!/usr/bin/env node`.
func toolEnv(bin string) []string {
	env := os.Environ()
	var prepend []string
	seen := map[string]bool{}
	addDir := func(p string) {
		d := filepath.Dir(p)
		if d != "" && d != "." && !seen[d] {
			seen[d] = true
			prepend = append(prepend, d)
		}
	}
	addDir(bin)
	if target, err := filepath.EvalSymlinks(bin); err == nil {
		addDir(target)
	}
	if len(prepend) == 0 {
		return env
	}
	path := os.Getenv("PATH")
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = "PATH=" + strings.Join(prepend, string(os.PathListSeparator)) +
				string(os.PathListSeparator) + path
			return env
		}
	}
	return append(env, "PATH="+strings.Join(prepend, string(os.PathListSeparator)))
}

// toolCmd — exec.Cmd с окружением, в котором инструмент точно запустится.
func toolCmd(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = toolEnv(bin)
	return cmd
}
