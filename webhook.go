package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

var webhookClient = &http.Client{Timeout: 10 * time.Second}

// webhookPayload is the JSON body for Slack/Mattermost incoming webhooks.
type webhookPayload struct {
	Text string `json:"text"`
}

// hostID returns a machine identifier for webhook messages.
// Uses FASTDEDUP_HOST_ID env var if set, otherwise os.Hostname().
func hostID() string {
	if id := os.Getenv("FASTDEDUP_HOST_ID"); id != "" {
		return id
	}
	if name, err := os.Hostname(); err == nil {
		return name
	}
	return "unknown"
}

// sendWebhook posts a message to a Slack/Mattermost incoming webhook URL.
func sendWebhook(url, text string) error {
	body, err := json.Marshal(webhookPayload{Text: text})
	if err != nil {
		return err
	}
	resp, err := webhookClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// notifyUpdate sends a run summary to the updates webhook.
func notifyUpdate(url, root string, stats *DedupStats, elapsed time.Duration, dryRun bool) {
	host := hostID()
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	msg := fmt.Sprintf("%s**[%s]** fastdedup `%s`\n", prefix, host, root)
	if stats.FilesDeduped > 0 || stats.Errors > 0 {
		msg += fmt.Sprintf("| deduped | saved | already | errors | elapsed |\n|---|---|---|---|---|\n| %s | %s | %s | %s | %s |",
			formatCount(stats.FilesDeduped),
			formatSize(stats.BytesSaved, false),
			formatCount(stats.AlreadyDeduped),
			formatCount(stats.Errors),
			elapsed)
	} else {
		msg += fmt.Sprintf("No changes — %s already deduped (%s)", formatCount(stats.AlreadyDeduped), elapsed)
	}
	if err := sendWebhook(url, msg); err != nil {
		slog.Debug("failed to send update webhook", "error", err)
	}
}

// notifyAlert sends a critical alert requiring user intervention.
func notifyAlert(url, root string, stats *DedupStats) {
	host := hostID()
	msg := fmt.Sprintf(":warning: **[%s]** fastdedup `%s`\n%s dedup errors — manual investigation recommended\nCheck error report: `~/.cache/fastdedup/report.txt`",
		host, root,
		formatCount(stats.Errors))
	if err := sendWebhook(url, msg); err != nil {
		slog.Debug("failed to send alert webhook", "error", err)
	}
}

// pingHealthcheck pings a healthchecks.io-style dead man's switch URL.
// A simple GET request signals that the job completed successfully.
func pingHealthcheck(url string) {
	resp, err := webhookClient.Get(url)
	if err != nil {
		slog.Debug("failed to ping healthcheck", "error", err)
		return
	}
	resp.Body.Close()
}
