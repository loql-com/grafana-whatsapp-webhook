package whatsapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/optiop/grafana-whatsapp-webhook/pkg/entity"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var (
	ErrWhatsAppUnavailable = errors.New("WhatsApp is not connected")
	errWhatsAppQueueFull   = errors.New("WhatsApp message queue is full")
	errQRPairingTimeout    = errors.New("WhatsApp QR pairing timed out")
)

// qrPairingRetryDelay is the pause between two QR pairing rounds after the
// previous round expired without the code being scanned.
const qrPairingRetryDelay = 5 * time.Second

type WhatsappService struct {
	client *whatsmeow.Client

	mu          sync.RWMutex
	ready       bool
	onLoggedOut func() // ends the current session; set by runSession

	cUserMessage,
	cGroupMessage chan entity.Message
}

func New() *WhatsappService {
	return &WhatsappService{
		cUserMessage:  make(chan entity.Message, 1024),
		cGroupMessage: make(chan entity.Message, 1024),
	}
}

func (ws *WhatsappService) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ws.setupWhatsappService(ctx); err != nil {
			log.Printf("WhatsApp initialization failed: %v", err)
		}
	}()
}

func (ws *WhatsappService) eventHandler(evt any) {
	switch v := evt.(type) {
	case *events.Message:
		log.Println("Received a message: ", v.Message.GetConversation())
	case *events.LoggedOut:
		log.Printf("WhatsApp session was logged out (reason: %s, on connect: %v); the linked device was removed", v.Reason, v.OnConnect)
		ws.mu.RLock()
		onLoggedOut := ws.onLoggedOut
		ws.mu.RUnlock()
		if onLoggedOut != nil {
			onLoggedOut()
		}
	}
}

// setupWhatsappService opens the SQLite device store and then runs WhatsApp
// sessions until ctx is cancelled. A session pairs (QR) or reconnects, serves
// the message queues and ends when the device is logged out remotely; the next
// session then starts fresh and offers a new QR code.
//
// If initialization fails, the function returns the error so the HTTP service can
// remain available for its liveness probe.
func (ws *WhatsappService) setupWhatsappService(ctx context.Context) error {
	if err := os.MkdirAll("data", os.ModePerm); err != nil {
		return err
	}

	debug := strings.ToLower(os.Getenv("APP_DEBUG")) == "true"
	level := "INFO"
	if debug {
		level = "DEBUG"
	}

	dbLog := waLog.Stdout("Database", level, true)
	container, err := sqlstore.New(ctx, "sqlite3", "data/sqlite3.db?_foreign_keys=on", dbLog)
	if err != nil {
		return err
	}
	clientLog := waLog.Stdout("Client", level, true)

	for {
		if err := ws.runSession(ctx, container, clientLog); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Println("WhatsApp session ended; starting a new one (a QR code will be offered if no device is linked)")
	}
}

// runSession runs one WhatsApp session. It returns nil when the session ended
// because ctx was cancelled or the device was logged out, and an error when the
// session could not be established.
func (ws *WhatsappService) runSession(ctx context.Context, container *sqlstore.Container, clientLog waLog.Logger) error {
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return err
	}

	client := whatsmeow.NewClient(deviceStore, clientLog)
	ws.client = client
	defer client.Disconnect()

	// sessionCtx ends when the parent ctx is cancelled or the device is logged out.
	sessionCtx, endSession := context.WithCancel(ctx)
	defer endSession()
	ws.mu.Lock()
	ws.onLoggedOut = endSession
	ws.mu.Unlock()

	// connectedCh is closed on the first events.Connected, signalling that the
	// WebSocket handshake (including any 515 server-redirect reconnect) is done.
	connectedCh := make(chan struct{})
	var connectedOnce sync.Once
	client.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			connectedOnce.Do(func() { close(connectedCh) })
		}
	})
	client.AddEventHandler(ws.eventHandler)

	if client.Store.ID == nil {
		// Not paired yet: keep offering fresh QR codes until one is scanned.
		// whatsmeow disconnects the client itself when a QR round times out,
		// so a new channel + Connect() starts a clean round.
		err := pairUntilSuccess(sessionCtx, qrPairingRetryDelay, func(ctx context.Context) error {
			qrChan, err := client.GetQRChannel(ctx)
			if err != nil {
				return err
			}
			if err := client.Connect(); err != nil {
				return err
			}
			return waitForQRPairing(ctx, qrChan, writeQRCode)
		})
		if err != nil {
			if sessionCtx.Err() != nil {
				return nil
			}
			return err
		}
	} else if err := client.Connect(); err != nil {
		return err
	}

	// Wait until the connection is fully established before querying groups.
	// This handles the 515 server-redirect reconnect that happens after pairing.
	select {
	case <-connectedCh:
	case <-sessionCtx.Done():
		return nil
	}

	groups, err := client.GetJoinedGroups(sessionCtx)
	if err != nil {
		log.Printf("failed to retrieve joined WhatsApp groups: %v", err)
	} else {
		log.Println("Joined groups:")
		for _, group := range groups {
			log.Println("Name: ", group.Name)
			log.Println("Jid: ", group.JID)
			log.Println("----------------")
		}
	}

	ws.setReady(true)
	defer ws.setReady(false)

	var senderWG sync.WaitGroup
	senderWG.Add(2)
	go func() {
		defer senderWG.Done()
		ws.handleSendUserMessages(sessionCtx)
	}()
	go func() {
		defer senderWG.Done()
		ws.handleSendGroupMessages(sessionCtx)
	}()
	<-sessionCtx.Done()
	senderWG.Wait()
	return nil
}

func waitForQRPairing(
	ctx context.Context,
	qrChan <-chan whatsmeow.QRChannelItem,
	onCode func(string),
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok := <-qrChan:
			if !ok {
				return errors.New("WhatsApp QR pairing channel closed before success")
			}
			switch evt.Event {
			case "code":
				onCode(evt.Code)
			case "success":
				return nil
			case "error":
				if evt.Error != nil {
					return evt.Error
				}
				return errors.New("WhatsApp QR pairing failed")
			case "timeout":
				return errQRPairingTimeout
			default:
				return fmt.Errorf("WhatsApp QR pairing failed: %s", evt.Event)
			}
		}
	}
}

// pairUntilSuccess runs one QR pairing round via attempt and repeats it after
// retryDelay whenever the round ended with errQRPairingTimeout. Any other error
// is returned immediately; a cancelled context ends the loop with ctx.Err().
func pairUntilSuccess(ctx context.Context, retryDelay time.Duration, attempt func(context.Context) error) error {
	for round := 1; ; round++ {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errQRPairingTimeout) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("WhatsApp QR pairing round %d expired without a scan; generating a new QR code", round)

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func writeQRCode(code string) {
	image, err := writeQRCodeLog(code, os.Stdout)
	if err != nil {
		log.Println("Error writing QR code log entry: ", err)
		return
	}
	if err := os.MkdirAll("out", 0o755); err != nil {
		log.Println("Error creating 'out' directory: ", err)
		return
	}
	if err := os.WriteFile("out/qr.png", image, 0o600); err != nil {
		log.Println("Error: to create QR code PNG file: ", err)
	}
}

func writeQRCodeLog(code string, output io.Writer) ([]byte, error) {
	image, err := qrcode.Encode(code, qrcode.Medium, 256)
	if err != nil {
		return nil, fmt.Errorf("encode QR code as PNG: %w", err)
	}

	entry := struct {
		Severity    string `json:"severity"`
		Event       string `json:"event"`
		Message     string `json:"message"`
		MIMEType    string `json:"mime_type"`
		ImageBase64 string `json:"image_base64"`
	}{
		Severity:    "INFO",
		Event:       "whatsapp_qr_code",
		Message:     "WhatsApp pairing QR code generated",
		MIMEType:    "image/png",
		ImageBase64: base64.StdEncoding.EncodeToString(image),
	}
	if err := json.NewEncoder(output).Encode(entry); err != nil {
		return nil, fmt.Errorf("write structured QR code log entry: %w", err)
	}
	return image, nil
}

func (ws *WhatsappService) SendNewWhatsAppMessageToUser(msg entity.Message) error {
	return ws.enqueue(ws.cUserMessage, msg)
}

func (ws *WhatsappService) SendNewWhatsAppMessageToGroup(msg entity.Message) error {
	return ws.enqueue(ws.cGroupMessage, msg)
}

func (ws *WhatsappService) enqueue(queue chan<- entity.Message, msg entity.Message) error {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	if !ws.ready {
		return ErrWhatsAppUnavailable
	}

	select {
	case queue <- msg:
		return nil
	default:
		return errWhatsAppQueueFull
	}
}

func (ws *WhatsappService) setReady(ready bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.ready = ready
}

func (ws *WhatsappService) handleSendUserMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case msg := <-ws.cUserMessage:
			to := types.NewJID(msg.To, "s.whatsapp.net")
			message := &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: &msg.Body,
				},
			}

			_, err := ws.client.SendMessage(ctx, to, message)
			if err != nil {
				log.Printf("failed to send user message to %s: %v", msg.To, err)
			}
		}
	}
}

func (ws *WhatsappService) handleSendGroupMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case msg := <-ws.cGroupMessage:
			groupJID := types.NewJID(msg.To, "g.us")
			message := &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: &msg.Body,
				},
			}

			_, err := ws.client.SendMessage(ctx, groupJID, message)
			if err != nil {
				log.Printf("failed to send group message to %s: %v", msg.To, err)
			}
		}
	}
}
