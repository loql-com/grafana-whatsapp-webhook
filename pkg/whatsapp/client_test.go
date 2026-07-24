package whatsapp

import (
	"context"
	"errors"
	"testing"

	"github.com/optiop/grafana-whatsapp-webhook/pkg/entity"
	"go.mau.fi/whatsmeow"
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
