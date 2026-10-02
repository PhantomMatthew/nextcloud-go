package imap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// append.go carries the M5 APPEND command (ADR-0108 §1): the save-to-Sent
// half of send. It is the client's FIRST command bearing a client literal,
// so it runs its own exchange (roundTripLiteral) — the M2 roundTrip treats
// every continuation as fatal, which stays the rule for all other commands.

// maxAppendSize caps one APPEND payload: the literal size is declared on
// the command line, so an over-limit message is rejected before any write
// (the same 25 MiB ceiling the mail layer's composer enforces).
const maxAppendSize = 25 << 20

// ErrAppendTooLarge reports an APPEND payload over maxAppendSize. Like the
// flag allowlist it fires before anything is written and the client stays
// usable.
var ErrAppendTooLarge = errors.New("imap: APPEND payload exceeds the 25 MiB limit")

// Append uploads msg to the named mailbox:
//
//	APPEND <quoted-name> (<flags>) "<INTERNALDATE>" {<n>}
//
// The wire name goes through quoteString (CR/LF injection-proof), the flags
// through the M4 allowlist (allowedFlag — a rejected flag is ErrInvalidFlag
// before any write), and the date renders in the RFC 3501 INTERNALDATE
// layout. Without LITERAL+ the payload (followed by the CRLF that completes
// the command) is written only after the server's "+ " continuation; a
// tagged NO/BAD arriving INSTEAD of the continuation aborts without a
// single payload byte written. With LITERAL+ the marker goes out as {n+}
// and the payload follows the command line immediately. Untagged lines
// during the exchange are collected and ignored. A tagged NO is
// ErrCommandRefused (and like every command failure breaks the client); BAD
// or a transport/protocol fault is fatal.
func (c *Client) Append(ctx context.Context, wireName string, flags []string, date time.Time, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(msg) > maxAppendSize {
		return ErrAppendTooLarge
	}
	for _, f := range flags {
		if !allowedFlag(f) {
			return fmt.Errorf("%w: %q", ErrInvalidFlag, f)
		}
	}
	q, err := quoteString(wireName)
	if err != nil {
		return err
	}
	nonSync := c.hasCap("LITERAL+")
	literal := "{" + strconv.Itoa(len(msg))
	if nonSync {
		literal += "+"
	}
	literal += "}"
	cmd := "APPEND " + q + " (" + strings.Join(flags, " ") + ") \"" + date.Format(internalDateLayout) + "\" " + literal
	resp, err := c.roundTripLiteral(cmd, msg, !nonSync)
	if err != nil {
		return err
	}
	switch resp.status {
	case "OK":
		return nil
	case "NO":
		return c.fail(withText(ErrCommandRefused, resp.text))
	default:
		return c.fail(fmt.Errorf("imap: APPEND: %s %s", resp.status, resp.text))
	}
}

// roundTripLiteral writes one tagged command carrying a client literal and
// reads to its tagged response. waitContinuation=true (the classic form):
// the payload plus its terminating CRLF is written only after the server's
// single "+ " continuation. waitContinuation=false (LITERAL+, the "{n+}"
// marker): the payload follows the command line immediately. A second
// continuation — or any continuation when none is expected — is the same
// fatal protocol violation roundTrip enforces for every other command.
func (c *Client) roundTripLiteral(cmd string, payload []byte, waitContinuation bool) (taggedResponse, error) {
	if c.broken {
		return taggedResponse{}, errBroken
	}
	if err := c.setDeadline(); err != nil {
		return taggedResponse{}, c.fail(err)
	}
	c.tagSeq++
	tag := "a" + strconv.Itoa(c.tagSeq)
	if _, err := io.WriteString(c.conn, tag+" "+cmd+"\r\n"); err != nil {
		return taggedResponse{}, c.fail(fmt.Errorf("imap: write command: %w", err))
	}
	sent := false
	if !waitContinuation {
		if err := c.writeLiteral(payload); err != nil {
			return taggedResponse{}, c.fail(err)
		}
		sent = true
	}
	var resp taggedResponse
	for {
		line, err := c.readLine()
		if err != nil {
			return taggedResponse{}, c.fail(fmt.Errorf("imap: read response: %w", err))
		}
		switch {
		case strings.HasPrefix(line, "* "):
			resp.untagged = append(resp.untagged, line)
		case strings.HasPrefix(line, "+ "):
			if sent {
				return taggedResponse{}, c.fail(fmt.Errorf("imap: unexpected continuation %q", line))
			}
			if err := c.writeLiteral(payload); err != nil {
				return taggedResponse{}, c.fail(err)
			}
			sent = true
		case strings.HasPrefix(line, tag+" "):
			status, text, _ := strings.Cut(strings.TrimPrefix(line, tag+" "), " ")
			switch status {
			case "OK", "NO", "BAD":
				resp.status, resp.text = status, text
				if err := c.conn.SetDeadline(time.Time{}); err != nil {
					return taggedResponse{}, c.fail(err)
				}
				return resp, nil
			default:
				return taggedResponse{}, c.fail(fmt.Errorf("imap: malformed tagged status in %q", line))
			}
		default:
			return taggedResponse{}, c.fail(fmt.Errorf("imap: malformed response line %q", line))
		}
	}
}

// writeLiteral writes the APPEND payload plus the CRLF that completes the
// command line the literal interrupted.
func (c *Client) writeLiteral(payload []byte) error {
	if _, err := c.conn.Write(payload); err != nil {
		return fmt.Errorf("imap: write literal: %w", err)
	}
	if _, err := io.WriteString(c.conn, "\r\n"); err != nil {
		return fmt.Errorf("imap: write literal terminator: %w", err)
	}
	return nil
}
