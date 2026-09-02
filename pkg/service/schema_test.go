package service

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func decodeFixture(t *testing.T, name string) GrafanaAlert {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var alert GrafanaAlert
	if err := json.Unmarshal(raw, &alert); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return alert
}

func TestTextPrefersGrafanaMessage(t *testing.T) {
	alert := GrafanaAlert{Title: "ignored", Message: "plain grafana message"}
	if got := alert.Text(); got != "plain grafana message" {
		t.Errorf("Text() = %q, want grafana message", got)
	}
}

func TestTextIsEmptyWithoutAnyContent(t *testing.T) {
	if got := (GrafanaAlert{}).Text(); got != "" {
		t.Errorf("Text() = %q, want empty", got)
	}
}

func TestTextFromOnCallEscalationPayload(t *testing.T) {
	alert := decodeFixture(t, "oncall_escalation.json")
	got := alert.Text()

	want := strings.Join([]string{
		"[FIRING] TestIncidentManagement",
		"TEST: Checkout API Error-Rate > 5%",
		"Dies ist ein Test-Alert. Kein echter Vorfall.",
		"https://grafana.example.com/a/grafana-oncall-app/alert-groups/I92K4UR5T78P7",
	}, "\n")
	if got != want {
		t.Errorf("Text() =\n%s\nwant:\n%s", got, want)
	}
}

func TestTextFromOnCallPayloadWithGrafanaMessage(t *testing.T) {
	body := `{"event":{"type":"resolve"},"alert_group":{"title":"Disk full","state":"resolved","permalinks":{"web":"https://oncall/ag/1"}},"alert_payload":{"title":"[RESOLVED] Disk full","message":"Disk usage back to 40%"}}`
	var alert GrafanaAlert
	if err := json.Unmarshal([]byte(body), &alert); err != nil {
		t.Fatal(err)
	}
	want := "[RESOLVED] Disk full\nDisk usage back to 40%\nhttps://oncall/ag/1"
	if got := alert.Text(); got != want {
		t.Errorf("Text() = %q, want %q", got, want)
	}
}

func TestTextFromOnCallPayloadWithoutAlertPayload(t *testing.T) {
	// OnCall sends alert_payload as "" when the alert group has no alerts.
	body := `{"event":{"type":"acknowledge"},"alert_group":{"title":"Something","state":"acknowledged","permalinks":{"web":""}},"alert_payload":""}`
	var alert GrafanaAlert
	if err := json.Unmarshal([]byte(body), &alert); err != nil {
		t.Fatal(err)
	}
	if got := alert.Text(); got != "[ACKNOWLEDGED] Something" {
		t.Errorf("Text() = %q", got)
	}
}
