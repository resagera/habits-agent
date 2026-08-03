package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeExec создаёт исполняемый файл-заглушку по указанному пути.
func makeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Главный сценарий: PATH урезан (как под systemd), codex лежит в nvm —
// находим его и берём САМУЮ СВЕЖУЮ версию node, а не первую по алфавиту.
func TestResolveToolFindsNewestNvm(t *testing.T) {
	home := t.TempDir()
	old := filepath.Join(home, ".nvm", "versions", "node", "v16.20.2", "bin", "codex")
	newer := filepath.Join(home, ".nvm", "versions", "node", "v20.19.6", "bin", "codex")
	makeExec(t, old)
	makeExec(t, newer)

	got := findIn(searchDirs(home), "codex")
	if got != newer {
		t.Fatalf("нашли %q, ожидали свежую версию %q", got, newer)
	}
}

func TestSearchDirsIncludesLocalBin(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "claude")
	makeExec(t, bin)

	if got := findIn(searchDirs(home), "claude"); got != bin {
		t.Fatalf("нашли %q, ожидали %q", got, bin)
	}
}

// Каталог с подходящим именем не должен сойти за бинарник.
func TestFindInSkipsDirsAndNonExecutable(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin", "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(home, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(plain), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("не исполняемый"), 0o644); err != nil {
		t.Fatal(err)
	}
	// только домашние каталоги: системные на машине с настоящим codex сорвали бы тест
	var own []string
	for _, d := range searchDirs(home) {
		if strings.HasPrefix(d, home) {
			own = append(own, d)
		}
	}
	if got := findIn(own, "codex"); got != "" {
		t.Fatalf("не должны были ничего найти, а нашли %q", got)
	}
}

func TestResolveToolEnvOverride(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-codex")
	makeExec(t, bin)
	t.Setenv("AI_AGENT_CODEX", bin)

	got, err := resolveTool("codex")
	if err != nil {
		t.Fatal(err)
	}
	if got != bin {
		t.Fatalf("нашли %q, ожидали %q", got, bin)
	}
}

func TestResolveToolEnvOverrideBadPath(t *testing.T) {
	t.Setenv("AI_AGENT_CODEX", filepath.Join(t.TempDir(), "нет-такого"))
	if _, err := resolveTool("codex"); err == nil {
		t.Fatal("ожидали ошибку про ненайденный файл")
	}
}

// Сообщение об ошибке должно подсказывать, где искали и как задать путь.
func TestResolveToolErrorMentionsEnvVar(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	_, err := resolveTool("codex")
	if err == nil {
		t.Fatal("ожидали ошибку")
	}
	if !strings.Contains(err.Error(), "AI_AGENT_CODEX") {
		t.Fatalf("в сообщении нет подсказки про переменную: %v", err)
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"v20.19.6", []int{20, 19, 6}},
		{"16.20.2", []int{16, 20, 2}},
		{"v22.0.0-rc1", []int{22, 0, 0}},
		{"installation", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := parseVersion(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%q: получили %v, ожидали %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q: получили %v, ожидали %v", c.in, got, c.want)
			}
		}
	}
}

func TestCompareVersions(t *testing.T) {
	if compareVersions([]int{20, 19, 6}, []int{16, 20, 2}) <= 0 {
		t.Fatal("20.19.6 должна быть больше 16.20.2")
	}
	if compareVersions([]int{20, 1}, []int{20, 1, 0}) != 0 {
		t.Fatal("20.1 и 20.1.0 — одна версия")
	}
	if compareVersions(nil, []int{1}) >= 0 {
		t.Fatal("версия без номера — самая младшая")
	}
}

// Без каталога бинарника в PATH nvm-установленный codex падает на своём
// шебанге `#!/usr/bin/env node` — проверяем, что каталог подставляется первым.
func TestToolEnvPrependsBinDir(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	makeExec(t, bin)
	t.Setenv("PATH", "/usr/bin")

	var path string
	for _, e := range toolEnv(bin) {
		if strings.HasPrefix(e, "PATH=") {
			path = strings.TrimPrefix(e, "PATH=")
		}
	}
	if !strings.HasPrefix(path, dir+string(os.PathListSeparator)) {
		t.Fatalf("PATH=%q не начинается с %q", path, dir)
	}
	if !strings.HasSuffix(path, "/usr/bin") {
		t.Fatalf("PATH=%q потерял исходное значение", path)
	}
}

// Симлинк (~/.local/bin/claude → …/versions/x.y.z) — в PATH должен попасть
// и каталог ссылки, и каталог цели.
func TestToolEnvFollowsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "versions", "2.1.220", "claude")
	makeExec(t, target)
	linkDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "claude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin")

	var path string
	for _, e := range toolEnv(link) {
		if strings.HasPrefix(e, "PATH=") {
			path = strings.TrimPrefix(e, "PATH=")
		}
	}
	if !strings.Contains(path, linkDir) || !strings.Contains(path, filepath.Dir(target)) {
		t.Fatalf("PATH=%q не содержит оба каталога (%s, %s)", path, linkDir, filepath.Dir(target))
	}
}

// Кэш не должен «залипать» на исчезнувшем пути.
func TestToolPathCacheRevalidates(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	makeExec(t, bin)
	t.Setenv("AI_AGENT_CODEX", bin)
	t.Cleanup(func() {
		binMu.Lock()
		delete(binCache, "codex")
		binMu.Unlock()
	})

	if _, err := toolPath("codex"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if got, err := toolPath("codex"); err == nil {
		t.Fatalf("удалённый бинарник вернулся из кэша: %q", got)
	}
}
