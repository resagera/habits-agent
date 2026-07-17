// habits-agent — крошечный агент мониторинга для страницы Servers.
// Отдаёт GET /metrics с JSON: ОС, внешний IP, диски, RAM, CPU, uptime.
// Только stdlib и /proc (Linux). Защита — токен в Authorization: Bearer.
//
// Два режима (можно совмещать):
//   - pull (по умолчанию): слушает AGENT_ADDR (:9101), бэкенд опрашивает сам;
//   - push (для машин без внешнего IP): при заданном AGENT_PUSH_URL сам шлёт
//     отчёт раз в AGENT_PUSH_INTERVAL (60s) с AGENT_TOKEN как Bearer.
//     HTTP-сервер в push-режиме поднимается, только если AGENT_ADDR задан явно.
//
// Запуск: AGENT_TOKEN=secret ./habits-agent
//    или: AGENT_TOKEN=<push-токен> AGENT_PUSH_URL=https://…/api/v1/agent/push ./habits-agent
package main

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Disk struct {
	Mount  string `json:"mount"`
	Device string `json:"device"`
	Total  uint64 `json:"total"`
	Free   uint64 `json:"free"`
}

type RAM struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
}

type Report struct {
	Hostname   string  `json:"hostname"`
	OS         string  `json:"os"`
	Kernel     string  `json:"kernel"`
	Arch       string  `json:"arch"`
	ExternalIP string  `json:"external_ip"`
	UptimeSec  int64   `json:"uptime_sec"`
	CPUPct     float64 `json:"cpu_pct"`
	CPUCores   int     `json:"cpu_cores"`
	Load1      float64 `json:"load1"`
	RAM        RAM     `json:"ram"`
	Disks      []Disk  `json:"disks"`
}

func main() {
	addr := os.Getenv("AGENT_ADDR")
	token := os.Getenv("AGENT_TOKEN")
	pushURL := os.Getenv("AGENT_PUSH_URL")

	if pushURL != "" {
		interval := 60 * time.Second
		if raw := os.Getenv("AGENT_PUSH_INTERVAL"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d >= 10*time.Second {
				interval = d
			}
		}
		go pushLoop(pushURL, token, interval)
		if addr == "" {
			select {} // push-only: порт не занимаем
		}
	}

	if addr == "" {
		addr = ":9101"
	}
	if token == "" {
		log.Println("WARN: AGENT_TOKEN is empty — metrics are public")
	}

	http.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(collect())
	})

	log.Printf("habits-agent listening on %s", addr)
	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// pushLoop раз в interval отправляет отчёт на бэкенд (режим для машин
// без внешнего IP). Ошибки не фатальны — следующий тик всё исправит.
func pushLoop(url, token string, interval time.Duration) {
	log.Printf("habits-agent: push mode, %s every %s", url, interval)
	// Без keep-alive: раз в минуту дешевле открыть новое соединение, чем
	// зависнуть на «полумёртвом» HTTP/2-коннекте (NAT/рестарт прокси убивают
	// его молча, а Go продолжает слать запросы в мёртвый канал до таймаута).
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	send := func() {
		body, err := json.Marshal(collect())
		if err != nil {
			log.Printf("push: marshal: %v", err)
			return
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			log.Printf("push: request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("push: %v", err)
			return
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("push: server replied %s", resp.Status)
		}
	}
	send()
	for range time.Tick(interval) {
		send()
	}
}

func collect() Report {
	r := Report{Arch: runtime.GOARCH, CPUCores: runtime.NumCPU()}
	r.Hostname, _ = os.Hostname()
	r.OS = osPrettyName()
	r.Kernel = firstLine("/proc/sys/kernel/osrelease")
	r.ExternalIP = externalIP()
	r.UptimeSec = uptime()
	r.CPUPct = cpuPercent()
	r.Load1 = load1()
	r.RAM = ram()
	r.Disks = disks()
	return r
}

func firstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
}

func osPrettyName() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}

func uptime() int64 {
	fields := strings.Fields(firstLine("/proc/uptime"))
	if len(fields) == 0 {
		return 0
	}
	sec, _ := strconv.ParseFloat(fields[0], 64)
	return int64(sec)
}

func load1() float64 {
	fields := strings.Fields(firstLine("/proc/loadavg"))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// cpuTimes возвращает (busy, total) из первой строки /proc/stat.
func cpuTimes() (uint64, uint64) {
	fields := strings.Fields(firstLine("/proc/stat"))
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var total, idle uint64
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return total - idle, total
}

func cpuPercent() float64 {
	busy1, total1 := cpuTimes()
	time.Sleep(300 * time.Millisecond)
	busy2, total2 := cpuTimes()
	if total2 <= total1 {
		return 0
	}
	return float64(busy2-busy1) / float64(total2-total1) * 100
}

func ram() RAM {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return RAM{}
	}
	defer f.Close()
	var total, available uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}
	return RAM{Total: total, Used: total - available, Available: available}
}

var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "vfat": true, "ntfs": true, "exfat": true,
}

func disks() []Disk {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{}
	var result []Disk
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !realFS[fields[2]] || seen[fields[0]] {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(fields[1], &st); err != nil {
			continue
		}
		seen[fields[0]] = true
		result = append(result, Disk{
			Mount:  fields[1],
			Device: fields[0],
			Total:  st.Blocks * uint64(st.Bsize),
			Free:   st.Bavail * uint64(st.Bsize),
		})
	}
	return result
}

var ipCache struct {
	sync.Mutex
	value string
	at    time.Time
}

func externalIP() string {
	ipCache.Lock()
	defer ipCache.Unlock()
	if ipCache.value != "" && time.Since(ipCache.at) < time.Hour {
		return ipCache.value
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		return ipCache.value
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil || resp.StatusCode != http.StatusOK {
		return ipCache.value
	}
	ip := strings.TrimSpace(string(body))
	if ip != "" {
		ipCache.value, ipCache.at = ip, time.Now()
	}
	return ipCache.value
}

