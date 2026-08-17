package portfolio

import (
	"errors"
	"strings"
	"testing"
)

func TestPublicErrorRedactsEndpointCredentials(t *testing.T) {
	err := errors.New(
		`Post "https://rpc-user:rpc-password@rpc.example/` +
			`FAKE_SENTIO_RPC_KEY_FOR_REDACTION_TEST?api_key=QUERYSECRET": context deadline exceeded`,
	)
	message := PublicError(err)
	for _, secret := range []string{
		"rpc-user",
		"rpc-password",
		"FAKE_SENTIO_RPC_KEY_FOR_REDACTION_TEST",
		"QUERYSECRET",
	} {
		if strings.Contains(message, secret) {
			t.Fatalf("public error leaked %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "context deadline exceeded") {
		t.Fatalf("public error lost the explicit failure reason: %s", message)
	}
}

func TestPublicErrorIsSingleLineAndBounded(t *testing.T) {
	message := PublicError(errors.New(strings.Repeat("x", 600) + "\nsecret second line"))
	if strings.Contains(message, "\n") {
		t.Fatalf("public error contains a newline: %q", message)
	}
	if len(message) > 500 {
		t.Fatalf("public error length = %d, want at most 500", len(message))
	}
}

func TestPublicErrorRedactsWebSocketEndpoint(t *testing.T) {
	message := PublicError(errors.New(`dial wss://rpc.example/SECRET: connection refused`))
	if strings.Contains(message, "SECRET") || strings.Contains(message, "rpc.example") {
		t.Fatalf("public error leaked WebSocket endpoint: %s", message)
	}
}
