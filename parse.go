package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Report è il contenuto di un singolo POST di Monit verso /collector.
// Monit invia sempre lo status completo di tutti i servizi; se il POST è
// stato scatenato da un evento, contiene in più un elemento <event>.
type Report struct {
	MonitID     string
	Incarnation int64
	Version     string
	Server      Server
	Platform    Platform
	Services    []Service
	Events      []Event
	// Groups: nome gruppo -> servizi, dalle direttive "group" del monitrc.
	Groups map[string][]string
}

type Server struct {
	Uptime        int64
	Poll          int64
	StartDelay    int64
	LocalHostname string
	ControlFile   string
	HttpdAddress  string
	HttpdPort     int64
	HttpdSSL      bool
}

type Platform struct {
	Name     string
	Release  string
	Version  string
	Machine  string
	CPU      int64
	MemoryKB int64
	SwapKB   int64
}

type Service struct {
	Name          string
	Type          int64
	CollectedAt   time.Time
	Status        int64 // bitmask degli eventi in errore, 0 = OK
	StatusHint    int64
	Monitor       int64 // 0 = non monitorato, 1 = monitorato, 2 = in inizializzazione
	MonitorMode   int64
	OnReboot      int64
	PendingAction int64
	Data          map[string]any // campi specifici del tipo (cpu, memoria, block, port, ...)
}

type Event struct {
	CollectedAt time.Time
	Service     string
	ServiceType int64
	ID          int64 // tipo di evento (bitmask Event_*), es. 0x200 = NonExist
	State       int64 // 0 succeeded, 1 failed, 2 changed, 3 changed not, 4 init
	Action      int64
	Message     string
}

// Tipi di servizio Monit (Service_Type).
var ServiceTypeNames = map[int64]string{
	0: "filesystem", 1: "directory", 2: "file", 3: "process", 4: "host",
	5: "system", 6: "fifo", 7: "program", 8: "net",
}

// Campi comuni a tutti i servizi: vanno in colonne dedicate, non in Data.
var commonServiceFields = map[string]bool{
	"name": true, "type": true, "collected_sec": true, "collected_usec": true,
	"status": true, "status_hint": true, "monitor": true, "monitormode": true,
	"onreboot": true, "pendingaction": true,
}

// Foglie da tenere sempre come stringa anche se sembrano numeri.
var stringLeaves = map[string]bool{"mode": true, "fsflags": true, "request": true}

// node è un albero XML generico: lo schema di Monit cambia tra le versioni
// e tra i tipi di servizio, quindi leggiamo tutto e poi estraiamo ciò che serve.
type node struct {
	name     string
	attrs    map[string]string
	text     strings.Builder
	children []*node
}

func (n *node) child(name string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

func (n *node) get(path ...string) *node {
	cur := n
	for _, p := range path {
		cur = cur.child(p)
	}
	return cur
}

func (n *node) str(path ...string) string {
	c := n.get(path...)
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.text.String())
}

func (n *node) int(path ...string) int64 {
	v, _ := strconv.ParseInt(n.str(path...), 10, 64)
	return v
}

func parseTree(body []byte) (*node, error) {
	// Monit dichiara ISO-8859-1 ma di fatto spedisce i byte così come sono
	// (spesso UTF-8). Se il body è UTF-8 valido lo usiamo tale e quale,
	// altrimenti lo convertiamo da Latin-1.
	if !utf8.Valid(body) {
		body = latin1ToUTF8(body)
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }

	var root *node
	var stack []*node
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{name: t.Name.Local}
			if len(t.Attr) > 0 {
				n.attrs = make(map[string]string, len(t.Attr))
				for _, a := range t.Attr {
					n.attrs[a.Name.Local] = a.Value
				}
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("più di un elemento radice")
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("documento vuoto")
	}
	return root, nil
}

func latin1ToUTF8(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/8)
	for _, c := range b {
		out = utf8.AppendRune(out, rune(c))
	}
	return out
}

func unixTime(sec, usec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, usec*1000).UTC()
}

// ParseReport interpreta il payload XML inviato da Monit.
func ParseReport(body []byte) (*Report, error) {
	root, err := parseTree(body)
	if err != nil {
		return nil, fmt.Errorf("xml non valido: %w", err)
	}
	if root.name != "monit" {
		return nil, fmt.Errorf("elemento radice inatteso <%s>", root.name)
	}
	r := &Report{
		MonitID: strings.TrimSpace(root.attrs["id"]),
		Version: root.attrs["version"],
	}
	r.Incarnation, _ = strconv.ParseInt(root.attrs["incarnation"], 10, 64)
	// Monit molto vecchi mettevano id/incarnation dentro <server>.
	if r.MonitID == "" {
		r.MonitID = root.str("server", "id")
		r.Incarnation = root.int("server", "incarnation")
		r.Version = root.str("server", "version")
	}
	if r.MonitID == "" {
		return nil, errors.New("monit id mancante")
	}
	if len(r.MonitID) > 64 {
		return nil, errors.New("monit id troppo lungo")
	}

	srv := root.child("server")
	r.Server = Server{
		Uptime:        srv.int("uptime"),
		Poll:          srv.int("poll"),
		StartDelay:    srv.int("startdelay"),
		LocalHostname: srv.str("localhostname"),
		ControlFile:   srv.str("controlfile"),
		HttpdAddress:  srv.str("httpd", "address"),
		HttpdPort:     srv.int("httpd", "port"),
		HttpdSSL:      srv.int("httpd", "ssl") == 1,
	}
	// Nota: <server><credentials> (utente/password dell'httpd di Monit) viene
	// ignorato di proposito: non lo salviamo.

	pl := root.child("platform")
	r.Platform = Platform{
		Name:     pl.str("name"),
		Release:  pl.str("release"),
		Version:  pl.str("version"),
		Machine:  pl.str("machine"),
		CPU:      pl.int("cpu"),
		MemoryKB: pl.int("memory"),
		SwapKB:   pl.int("swap"),
	}

	for _, s := range root.get("services").childrenNamed("service") {
		svc := Service{
			Name:          s.attrs["name"],
			CollectedAt:   unixTime(s.int("collected_sec"), s.int("collected_usec")),
			Status:        s.int("status"),
			StatusHint:    s.int("status_hint"),
			Monitor:       s.int("monitor"),
			MonitorMode:   s.int("monitormode"),
			OnReboot:      s.int("onreboot"),
			PendingAction: s.int("pendingaction"),
		}
		if svc.Name == "" { // formato pre-5.x: <name> come figlio
			svc.Name = s.str("name")
		}
		if t := s.str("type"); t != "" {
			svc.Type, _ = strconv.ParseInt(t, 10, 64)
		} else {
			svc.Type, _ = strconv.ParseInt(s.attrs["type"], 10, 64)
		}
		if svc.Name == "" {
			continue
		}
		svc.Data = make(map[string]any)
		for k, v := range childrenToValues(s.children, commonServiceFields) {
			svc.Data[k] = v
		}
		r.Services = append(r.Services, svc)
	}

	// <servicegroups><servicegroup name="H24"><service>nginx</service>...</servicegroup></servicegroups>
	for _, g := range root.get("servicegroups").childrenNamed("servicegroup") {
		name := strings.TrimSpace(g.attrs["name"])
		if name == "" {
			name = g.str("name") // per sicurezza, se in qualche versione fosse un figlio
		}
		if name == "" {
			continue
		}
		if r.Groups == nil {
			r.Groups = make(map[string][]string)
		}
		for _, sv := range g.childrenNamed("service") {
			if n := strings.TrimSpace(sv.text.String()); n != "" {
				r.Groups[name] = append(r.Groups[name], n)
			}
		}
	}

	for _, e := range root.childrenNamed("event") {
		r.Events = append(r.Events, Event{
			CollectedAt: unixTime(e.int("collected_sec"), e.int("collected_usec")),
			Service:     e.str("service"),
			ServiceType: e.int("type"),
			ID:          e.int("id"),
			State:       e.int("state"),
			Action:      e.int("action"),
			Message:     e.str("message"),
		})
	}
	return r, nil
}

func (n *node) childrenNamed(name string) []*node {
	if n == nil {
		return nil
	}
	var out []*node
	for _, c := range n.children {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// childrenToValues converte i figli in una mappa; elementi ripetuti
// (es. più <port> o <icmp> in un check host) diventano un array.
func childrenToValues(children []*node, skip map[string]bool) map[string]any {
	count := map[string]int{}
	for _, c := range children {
		count[c.name]++
	}
	out := make(map[string]any)
	for _, c := range children {
		if skip[c.name] {
			continue
		}
		v := nodeValue(c)
		if count[c.name] > 1 {
			arr, _ := out[c.name].([]any)
			out[c.name] = append(arr, v)
		} else {
			out[c.name] = v
		}
	}
	return out
}

func nodeValue(n *node) any {
	if len(n.children) == 0 {
		s := strings.TrimSpace(n.text.String())
		if stringLeaves[n.name] {
			return s
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		// niente NaN/Inf: non sono serializzabili in JSON
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
		return s
	}
	return childrenToValues(n.children, nil)
}

// FailedCount conta i servizi monitorati che sono in errore.
func (r *Report) FailedCount() int {
	n := 0
	for _, s := range r.Services {
		if s.Status != 0 {
			n++
		}
	}
	return n
}
