package main

import (
	"encoding/json"
	"os"
	"strings"
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
		t.Fatalf("mode must stay a string: %#v", by["root"].Data["mode"])
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
		t.Fatal("reports carrying an event must also contain the full status")
	}
}

func TestLatin1AndRepeated(t *testing.T) {
	// byte 0xE9 = 'é' in ISO-8859-1, not valid UTF-8; two repeated <port> elements.
	body := []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><monit id=\"abc\" incarnation=\"1\" version=\"5.0\">" +
		"<server><localhostname>caf\xe9</localhostname></server><services>" +
		"<service name=\"web\"><type>4</type><port><portnumber>80</portnumber></port><port><portnumber>443</portnumber></port></service>" +
		"</services></monit>")
	r, err := ParseReport(body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Server.LocalHostname != "café" {
		t.Fatalf("latin1: %q", r.Server.LocalHostname)
	}
	ports, ok := r.Services[0].Data["port"].([]any)
	if !ok || len(ports) != 2 {
		t.Fatalf("repeated port: %#v", r.Services[0].Data["port"])
	}
}

func TestOldFormat(t *testing.T) {
	// old Monit: <service type="3"><name>..</name>
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
			t.Fatalf("expected an error for %q", b)
		}
	}
}

func TestParseGroups(t *testing.T) {
	r := load(t, "groups.xml")
	h24 := r.Groups["H24"]
	if len(h24) != 2 || h24[0] != "root" || h24[1] != "sshd" {
		t.Fatalf("H24: %#v", r.Groups)
	}
	if len(r.Groups["web"]) != 1 || r.Groups["web"][0] != "root" {
		t.Fatalf("web: %#v", r.Groups)
	}
	if len(load(t, "status.xml").Groups) != 0 {
		t.Fatal("without groups the map must be empty")
	}
}

func TestParseCredentials(t *testing.T) {
	b, err := os.ReadFile("testdata/status.xml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Server.HttpdUser != "" || r.Server.HttpdPassword != "" {
		t.Fatalf("no credentials expected: %+v", r.Server)
	}
	// Same report as sent without "register without credentials".
	withCreds := strings.Replace(string(b), "</httpd>",
		"</httpd><credentials><username>admin</username><password>s3cr&amp;t</password></credentials>", 1)
	r, err = ParseReport([]byte(withCreds))
	if err != nil {
		t.Fatal(err)
	}
	if r.Server.HttpdUser != "admin" || r.Server.HttpdPassword != "s3cr&t" {
		t.Fatalf("credentials: %q %q", r.Server.HttpdUser, r.Server.HttpdPassword)
	}
}

func TestCredentialSealer(t *testing.T) {
	plain, _ := newCredentialSealer("")
	v, _ := plain.Seal("pw", "id1")
	if v != "plain:pw" {
		t.Fatalf("plain: %q", v)
	}
	if v, _ := plain.Seal("", "id1"); v != "" {
		t.Fatalf("empty password must stay empty: %q", v)
	}

	key := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // "0123456789abcdef0123456789abcdef"
	c, err := newCredentialSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := c.Seal("pa:ss wörd", "id1")
	v2, _ := c.Seal("pa:ss wörd", "id1")
	if !strings.HasPrefix(v1, "aes256gcm:") || v1 == v2 {
		t.Fatalf("expected random nonces: %q %q", v1, v2)
	}
	if got, err := c.Open(v1, "id1"); err != nil || got != "pa:ss wörd" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := c.Open(v1, "id2"); err == nil {
		t.Fatal("ciphertext must be bound to the monit_id")
	}
	other, _ := newCredentialSealer("ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")
	if _, err := other.Open(v1, "id1"); err == nil {
		t.Fatal("wrong key must fail")
	}
	if _, err := newCredentialSealer("c2hvcnQ="); err == nil {
		t.Fatal("short key must be rejected")
	}
}
