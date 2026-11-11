package main

import (
	"encoding/json"
	"os"
	"testing"
)

func load(t *testing.T, name string) *Report {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseReport(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseStatus(t *testing.T) {
	r := load(t, "status.xml")
	if r.MonitID != "8be8007f834c19bf288fc1021a2e4d42" || r.Version != "5.33.0" || r.Incarnation == 0 {
		t.Fatalf("header: %+v", r)
	}
	if r.Server.LocalHostname != "vm" || r.Server.Poll != 5 || r.Server.HttpdPort != 2812 {
		t.Fatalf("server: %+v", r.Server)
	}
	if r.Platform.Name != "Linux" || r.Platform.CPU != 2 || r.Platform.MemoryKB == 0 {
		t.Fatalf("platform: %+v", r.Platform)
	}
	if len(r.Services) != 5 || len(r.Events) != 0 {
		t.Fatalf("services=%d events=%d", len(r.Services), len(r.Events))
	}
	by := map[string]Service{}
	for _, s := range r.Services {
		by[s.Name] = s
	}
	sys := by["vm"]
	if sys.Type != 5 || sys.CollectedAt.IsZero() {
		t.Fatalf("system: %+v", sys)
	}
	load := sys.Data["system"].(map[string]any)["load"].(map[string]any)
	if _, ok := load["avg01"].(float64); !ok {
		t.Fatalf("load avg01: %#v", load["avg01"])
	}
	if by["root"].Data["mode"] != "755" {
		t.Fatalf("mode deve restare stringa: %#v", by["root"].Data["mode"])
	}
	if by["local"].Status != 32 { // Event_Connection
		t.Fatalf("host check: %+v", by["local"])
	}
	if by["sshd"].Data["pid"].(int64) == 0 {
		t.Fatalf("pid")
	}
	if _, err := json.Marshal(r.Services); err != nil {
		t.Fatal(err)
	}
}

func TestParseEvent(t *testing.T) {
	r := load(t, "event.xml")
	if len(r.Events) != 1 {
		t.Fatalf("events=%d", len(r.Events))
	}
	e := r.Events[0]
	if e.Service != "sshd" || e.ID != 512 || e.State != 1 || e.Message != "process is not running" {
		t.Fatalf("event: %+v", e)
	}
	if len(r.Services) == 0 {
		t.Fatal("anche i report con evento contengono lo status completo")
	}
}

func TestLatin1AndRepeated(t *testing.T) {
	// byte 0xE0 = 'à' in ISO-8859-1, non UTF-8 valido; due <port> ripetuti.
	body := []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><monit id=\"abc\" incarnation=\"1\" version=\"5.0\">" +
		"<server><localhostname>citt\xe0</localhostname></server><services>" +
		"<service name=\"web\"><type>4</type><port><portnumber>80</portnumber></port><port><portnumber>443</portnumber></port></service>" +
		"</services></monit>")
	r, err := ParseReport(body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Server.LocalHostname != "città" {
		t.Fatalf("latin1: %q", r.Server.LocalHostname)
	}
	ports, ok := r.Services[0].Data["port"].([]any)
	if !ok || len(ports) != 2 {
		t.Fatalf("port ripetuti: %#v", r.Services[0].Data["port"])
	}
}

func TestOldFormat(t *testing.T) {
	// Monit vecchi: <service type="3"><name>..</name>
	body := []byte(`<?xml version="1.0" encoding="ISO-8859-1"?><monit><server><id>old1</id><incarnation>5</incarnation><version>5.2</version><localhostname>h</localhostname></server>` +
		`<services><service type="3"><name>nginx</name><status>0</status></service></services></monit>`)
	r, err := ParseReport(body)
	if err != nil {
		t.Fatal(err)
	}
	if r.MonitID != "old1" || r.Services[0].Name != "nginx" || r.Services[0].Type != 3 {
		t.Fatalf("%+v %+v", r, r.Services)
	}
}

func TestRejectGarbage(t *testing.T) {
	for _, b := range []string{"", "not xml", "<foo/>", `<monit></monit>`} {
		if _, err := ParseReport([]byte(b)); err == nil {
			t.Fatalf("atteso errore per %q", b)
		}
	}
}
