package whatsapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"testing"

	"github.com/optiop/grafana-whatsapp-webhook/pkg/entity"
	"go.mau.fi/whatsmeow"
	whatsmeowevents "go.mau.fi/whatsmeow/types/events"
)

func TestNewDoesNotConnectToWhatsApp(t *testing.T) {
	service := New()

	if service.client != nil {
		t.Fatal("New() must not initialize a WhatsApp client")
	}
}

func TestSendToUserReturnsUnavailableBeforeWhatsAppIsConnected(t *testing.T) {
	err := New().SendNewWhatsAppMessageToUser(entity.Message{To: "1234567890", Body: "test"})

	if !errors.Is(err, ErrWhatsAppUnavailable) {
		t.Fatalf("got error %v, want ErrWhatsAppUnavailable", err)
	}
}

func TestWaitForQRPairingReturnsErrorForTerminalFailure(t *testing.T) {
	qrChan := make(chan whatsmeow.QRChannelItem, 1)
	qrChan <- whatsmeow.QRChannelTimeout
	close(qrChan)

	err := waitForQRPairing(context.Background(), qrChan, func(string) {})

	if err == nil {
		t.Fatal("expected QR pairing timeout error")
	}
}

func TestWaitForQRPairingHandlesEachCodeOnce(t *testing.T) {
	qrChan := make(chan whatsmeow.QRChannelItem, 3)
	qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "first"}
	qrChan <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "second"}
	qrChan <- whatsmeow.QRChannelSuccess
	close(qrChan)

	var codes []string
	err := waitForQRPairing(context.Background(), qrChan, func(code string) {
		codes = append(codes, code)
	})

	if err != nil {
		t.Fatalf("waitForQRPairing returned an error: %v", err)
	}
	if len(codes) != 2 || codes[0] != "first" || codes[1] != "second" {
		t.Fatalf("got codes %v, want [first second]", codes)
	}
}

func TestWriteQRCodeLogWritesSingleBase64PNGEntry(t *testing.T) {
	var output bytes.Buffer

	_, err := writeQRCodeLog("test-code", &output)

	if err != nil {
		t.Fatalf("writeQRCodeLog returned an error: %v", err)
	}
	if got := bytes.Count(output.Bytes(), []byte{'\n'}); got != 1 {
		t.Fatalf("got %d physical log lines, want 1", got)
	}

	var entry struct {
		Event       string `json:"event"`
		MIMEType    string `json:"mime_type"`
		ImageBase64 string `json:"image_base64"`
	}
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("log entry is not valid JSON: %v", err)
	}
	if entry.Event != "whatsapp_qr_code" {
		t.Fatalf("got event %q, want whatsapp_qr_code", entry.Event)
	}
	if entry.MIMEType != "image/png" {
		t.Fatalf("got MIME type %q, want image/png", entry.MIMEType)
	}

	image, err := base64.StdEncoding.DecodeString(entry.ImageBase64)
	if err != nil {
		t.Fatalf("image_base64 is not valid base64: %v", err)
	}
	if _, err := png.DecodeConfig(bytes.NewReader(image)); err != nil {
		t.Fatalf("decoded image is not a valid PNG: %v", err)
	}
}

func TestWaitForQRPairingReturnsTimeoutSentinel(t *testing.T) {
	qrChan := make(chan whatsmeow.QRChannelItem, 1)
	qrChan <- whatsmeow.QRChannelTimeout
	close(qrChan)

	err := waitForQRPairing(context.Background(), qrChan, func(string) {})

	if !errors.Is(err, errQRPairingTimeout) {
		t.Fatalf("got %v, want errQRPairingTimeout", err)
	}
}

func TestPairUntilSuccessRetriesAfterTimeout(t *testing.T) {
	attempts := 0
	err := pairUntilSuccess(context.Background(), 0, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errQRPairingTimeout
		}
		return nil
	})

	if err != nil {
		t.Fatalf("pairUntilSuccess returned %v", err)
	}
	if attempts != 3 {
		t.Fatalf("got %d attempts, want 3", attempts)
	}
}

func TestPairUntilSuccessStopsOnOtherErrors(t *testing.T) {
	boom := errors.New("boom")
	attempts := 0
	err := pairUntilSuccess(context.Background(), 0, func(context.Context) error {
		attempts++
		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	if attempts != 1 {
		t.Fatalf("got %d attempts, want 1", attempts)
	}
}

func TestPairUntilSuccessStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	err := pairUntilSuccess(ctx, 0, func(context.Context) error {
		cancel()
		return errQRPairingTimeout
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestLoggedOutEventEndsItsOwnSession(t *testing.T) {
	ended := false
	handler := sessionEventHandler(func() { ended = true })

	handler(&whatsmeowevents.Connected{})
	if ended {
		t.Fatal("only LoggedOut may end the session")
	}

	handler(&whatsmeowevents.LoggedOut{OnConnect: false, Reason: whatsmeowevents.ConnectFailureLoggedOut})
	if !ended {
		t.Fatal("LoggedOut event must end the session it belongs to")
	}
}
