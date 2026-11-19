// Приёмка MPD-прокси на живой колонке v2: один запрос — одно чтение.
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const addr = "127.0.0.1:6600"

var ok = true

func check(name string, cond bool, detail ...any) {
	if cond {
		fmt.Println("PASS", name)
	} else {
		ok = false
		fmt.Println("FAIL", name, fmt.Sprint(detail...))
	}
}

var cur net.Conn
var br *bufio.Reader

func conn() {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Println("FAIL dial:", err)
		os.Exit(1)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	g, _ := bufio.NewReader(c).ReadString('\n')
	check("banner", strings.HasPrefix(g, "OK MPD"), g)
	cur = c
	br = bufio.NewReader(c)
}

func send(cmd string) {
	fmt.Fprintf(os.Stderr, "%s SEND %s\n", stamp(), cmd)
	fmt.Fprintf(cur, "%s\n", cmd)
}

func resp() []string {
	var out []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			out = append(out, "ERR:"+err.Error())
			break
		}
		fmt.Fprintf(os.Stderr, "%s RECV %q\n", stamp(), l)
		out = append(out, l)
		if l == "OK\n" || strings.HasPrefix(l, "ACK ") {
			return out
		}
	}
	return out
}

func stamp() string { return time.Now().Format("15:04:05.000") }

// statusVol — один status-запрос, возвращает (volume, полный ответ).
func statusVol() (string, []string) {
	fmt.Fprintf(os.Stderr, "%s SEND status\n", stamp())
	send("status")
	r := resp()
	for _, l := range r {
		if strings.HasPrefix(l, "volume:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "volume:")), r
		}
	}
	return "ABSENT", r
}

func pactl(args ...string) {
	if err := exec.Command("pactl", args...).Run(); err != nil {
		fmt.Println("pactl:", args, err)
	}
}

func sinkPct() string {
	out, err := exec.Command("pactl", "get-sink-volume", "@DEFAULT_SINK@").Output()
	if err != nil {
		return "ERR"
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, "%") {
			return f
		}
	}
	return "?"
}

func main() {
	pactl("set-sink-volume", "@DEFAULT_SINK@", "75%") // старт = визуал 50
	time.Sleep(300 * time.Millisecond)

	conn()

	v, r := statusVol()
	check("status-has-volume-50", v == "50", fmt.Sprint(r))

	send("ping")
	r = resp()
	check("passthrough-ping", fmt.Sprint(r) == fmt.Sprint([]string{"OK\n"}), fmt.Sprint(r))

	send("setvol 60")
	r = resp()
	check("setvol-OK", fmt.Sprint(r) == fmt.Sprint([]string{"OK\n"}), fmt.Sprint(r))
	time.Sleep(400 * time.Millisecond)
	check("setvol-sink-90", sinkPct() == "90%", sinkPct())

	v, r = statusVol()
	check("status-volume-60", v == "60", v, fmt.Sprint(r))

	// idle-нотификация: второе соединение в idle, громкость меняется первым
	bc, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Println("FAIL dial-b:", err)
		os.Exit(1)
	}
	bc.SetDeadline(time.Now().Add(5 * time.Second))
	bbr := bufio.NewReader(bc)
	bbr.ReadString('\n') // баннер
	fmt.Fprintf(bc, "idle mixer player\n")
	time.Sleep(300 * time.Millisecond)
	send("setvol 40")
	resp() //consume OK — иначе поток сдвинется на один ответ
	l1, err := bbr.ReadString('\n')
	check("idle-changed", err == nil && l1 == "changed: mixer\n", l1, err)
	l2, err := bbr.ReadString('\n')
	check("idle-OK", err == nil && l2 == "OK\n", l2, err)

	// noidle
	fmt.Fprintf(bc, "idle\n")
	time.Sleep(200 * time.Millisecond)
	fmt.Fprintf(bc, "noidle\n")
	r = resp2(bbr)
	check("noidle", fmt.Sprint(r) == fmt.Sprint([]string{"OK\n"}), fmt.Sprint(r))
	bc.Close()

	// mute-путь: setvol 0
	send("setvol 0")
	r = resp()
	check("mute-OK", fmt.Sprint(r) == fmt.Sprint([]string{"OK\n"}), fmt.Sprint(r))
	time.Sleep(400 * time.Millisecond)
	check("mute-sink-0", sinkPct() == "0%", sinkPct())

	// volume относительная от ТОЧНОГО текущего: узнали по status
	v, _ = statusVol()
	send("volume +10")
	r = resp()
	check("volume-rel-OK", fmt.Sprint(r) == fmt.Sprint([]string{"OK\n"}), fmt.Sprint(r))
	// +10 визуальных → fromVisual→snapPct: от 0 это физ 20% = визуал 13;
	// проверяем физику sink на сетке
	if v == "0" {
		time.Sleep(400 * time.Millisecond)
		check("status-volume-rel-snap", sinkPct() == "20%", sinkPct(), "было визуал", v)
	} else {
		v2, _ := statusVol()
		check("status-volume-rel", v2 != "ABSENT" && v2 != v, "было", v, "стало", v2)
	}

	// возврат громкости к визуал 50
	send("setvol 50")
	resp()
	time.Sleep(400 * time.Millisecond)
	cur.Close()

	out, _ := exec.Command("netstat", "-tnp").Output()
	est := strings.Count(string(out), "127.0.0.1:6600")
	fmt.Printf("INFO соединений к 6600: %d\n", est)

	fmt.Println("ИТОГ:", map[bool]string{true: "ВСЁ ЗЕЛЁНОЕ", false: "ЕСТЬ ПРОВАЛЫ"}[ok])
	if !ok {
		os.Exit(1)
	}
}

func resp2(br *bufio.Reader) []string {
	var out []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return append(out, "ERR:"+err.Error())
		}
		out = append(out, l)
		if l == "OK\n" || strings.HasPrefix(l, "ACK ") {
			return out
		}
	}
}
