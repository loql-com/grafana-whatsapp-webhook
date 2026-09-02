package service

import (
	"encoding/json"
	"strings"
)

type CommonLabels struct {
	Alertname     string `json:"alertname"`
	GrafanaFolder string `json:"grafana_folder"`
	Phone         string `json:"phone"`
	RefID         string `json:"ref_id"`
}

// OnCallEvent is the "event" object of a Grafana OnCall outgoing webhook payload.
type OnCallEvent struct {
	Type string `json:"type"`
}

// OnCallPermalinks holds the links OnCall attaches to an alert group.
type OnCallPermalinks struct {
	Web string `json:"web"`
}

// OnCallAlertGroup is the "alert_group" object of a Grafana OnCall outgoing webhook payload.
type OnCallAlertGroup struct {
	Title      string           `json:"title"`
	State      string           `json:"state"`
	Permalinks OnCallPermalinks `json:"permalinks"`
}

// onCallAlertPayload is the subset of the raw alert ("alert_payload") that OnCall forwards.
// For alerts originating from Grafana Alerting it looks like a regular Grafana webhook payload.
type onCallAlertPayload struct {
	Title   string `json:"title"`
	Message string `json:"message"`
	Alerts  []struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

// GrafanaAlert represents an incoming alert.
//
// It accepts two payload shapes:
//   - a Grafana Alerting webhook payload (top-level "message" is used as-is), and
//   - a Grafana OnCall outgoing webhook payload with "Forward all" enabled
//     ("event", "alert_group", "alert_payload"), from which a message is composed.
type GrafanaAlert struct {
	Receiver     string       `json:"receiver"`
	Status       string       `json:"status"`
	CommonLabels CommonLabels `json:"commonLabels"`
	State        string       `json:"state"`
	Title        string       `json:"title"`
	Message      string       `json:"message"`

	Event        *OnCallEvent      `json:"event,omitempty"`
	AlertGroup   *OnCallAlertGroup `json:"alert_group,omitempty"`
	AlertPayload json.RawMessage   `json:"alert_payload,omitempty"`
}

// Text returns the message to send to WhatsApp, or "" if the payload carries no usable content.
func (a GrafanaAlert) Text() string {
	if a.Message != "" {
		return a.Message
	}
	if a.AlertGroup == nil {
		return ""
	}

	var lines []string
	addLine := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, existing := range lines {
			if existing == s {
				return
			}
		}
		lines = append(lines, s)
	}

	title := strings.TrimSpace(a.AlertGroup.Title)
	if state := strings.TrimSpace(a.AlertGroup.State); state != "" {
		title = strings.TrimSpace("[" + strings.ToUpper(state) + "] " + title)
	}
	addLine(title)

	var payload onCallAlertPayload
	// alert_payload is "" when the alert group has no alerts; ignore anything that is not an object.
	if len(a.AlertPayload) > 0 && a.AlertPayload[0] == '{' && json.Unmarshal(a.AlertPayload, &payload) == nil {
		if payload.Message != "" {
			addLine(payload.Message)
		} else {
			for _, alert := range payload.Alerts {
				addLine(alert.Annotations["summary"])
				addLine(alert.Annotations["description"])
			}
		}
	}

	addLine(a.AlertGroup.Permalinks.Web)

	return strings.Join(lines, "\n")
}
