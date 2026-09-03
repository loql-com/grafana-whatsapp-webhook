# Grafana Whatsapp Webhook

[![Go Report Card](https://goreportcard.com/badge/github.com/optiop/grafana-whatsapp-webhook)](https://goreportcard.com/report/github.com/optiop/grafana-whatsapp-webhook)
[![Slack Community](https://badgen.net/badge/icon/slack?icon=slack&label)](https://join.slack.com/t/optioporg/shared_invite/zt-33axtzuao-Kd5NzaVm2GOhozBHOTj_Yg)

This repository acts as a webhook service that listens for Grafana alerts and forwards them to a specified WhatsApp number or group. It enables seamless integration of Grafana alerts with WhatsApp for real-time notifications.

## Setup and Usage

### 1. Generate a Secret Token
Before using this service, **generate a secret token**. This is a user-defined value used to authenticate incoming webhook requests — it is **not** a Meta or WhatsApp API token.

### 2. Configure the `.env` File
Copy `.env.example` to `.env` and fill in your values:
```
WEBHOOK_SECRET=your_generated_secret
WHATSAPP_USER_ID=your_phone_number_without_plus
WHATSAPP_GROUP_ID=your_group_jid
```
> **Note:** The phone number must be entered **without the `+` sign**.

### 3. Run the Docker Container
```bash
docker run -p 8080:8080 \
  --env-file .env \
  --name grafana-whatsapp-webhook \
  --rm \
  -v ./data:/app/data \
  -v ./out:/app/out \
  -d europe-west1-docker.pkg.dev/ocfood-platform/common-docker-images/grafana-whatsapp-webhook:latest
```

> The `-v ./data:/app/data` mount persists your WhatsApp session so you don't need to re-authenticate after a restart.

### 4. Authenticate with WhatsApp
- **QR Code Generation:**
  - A QR code will be generated and saved to `./out/qr.png`.
  - Alternatively, retrieve it from the container logs:
    ```bash
    docker logs grafana-whatsapp-webhook
    ```

- **Link Device:**
  - Open WhatsApp, go to **Linked Devices**, and select **Link a Device**.
  - Scan the QR code from `./out/qr.png` or from the logs.

> **Note:** As long as no device is linked, the service keeps generating a fresh QR code
> (each one is valid for ~20-60 s, a new one follows after a short pause). Just grab the latest
> one from the logs whenever you are ready to scan. On Cloud Run, extract it with:
> ```bash
> gcloud logging read 'resource.labels.service_name="grafana-whatsapp-webhook" jsonPayload.event="whatsapp_qr_code"' \
>   --project=<project> --limit=1 --format='value(jsonPayload.image_base64)' | base64 -d > qr.png
> ```

### 5. Retrieve the Group JID (for Group Alerts)
Once authentication is complete, stream logs to retrieve the WhatsApp **Group JID**:
```bash
docker logs -f grafana-whatsapp-webhook
```
Copy the **JID** of the group where you want to send alerts —
**use only the number, not the `@g.us` suffix**.

**In this example ![Scan QR Code](images/jid.png) `120363400930729957` is the group ID.**

### 6. Configure Grafana Contact Points
Set up a **contact point** in Grafana with the following settings:

- **For a user:**
  - URL: `http://<your_ip>:8080/whatsapp/send/grafana-alert/user/<phone_number>`
  - HTTP Method: POST
  - Authorization Header - Scheme: `Bearer`
  - Authorization Header - Credentials: `<WEBHOOK_SECRET>`

- **For a group:**
  - URL: `http://<your_ip>:8080/whatsapp/send/grafana-alert/group/<group_id>`
  - HTTP Method: POST
  - Authorization Header - Scheme: `Bearer`
  - Authorization Header - Credentials: `<WEBHOOK_SECRET>`

Replace `<your_ip>`, `<phone_number>`, `<group_id>`, and `<WEBHOOK_SECRET>` with your actual values.

### 7. (Optional) Trigger from Grafana OnCall
The same endpoints also accept the payload of a **Grafana OnCall outgoing webhook**
(`event` / `alert_group` / `alert_payload`), so you can call this service from an escalation chain:

- URL: same as above
- HTTP method: `POST`
- Authorization header: `Bearer <WEBHOOK_SECRET>`
- **Forward all** enabled (default) — no custom data template needed.

The WhatsApp message is composed from the alert group state and title, the alert's
`message` (or the `summary`/`description` annotations) and the alert group permalink.
If a top-level `message` field is present (Grafana Alerting payload or a custom template), it is used as-is.

> **Note:** Do not use `localhost` or `127.0.0.1` — use the actual IP address of your machine.

> **⚠️ WARNING:** If you stop using this service, **unlink the device from WhatsApp** to maintain your account's security.
