package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// MailProvider supplies the "mail" capability block (ADR-0108): clients
// learn the Mail app exists on this server. Registered only when
// mail.enabled is on — the M1 surface is accounts-only, so the block is
// intentionally minimal (sync/send capabilities land with their epics).
type MailProvider struct{}

func (MailProvider) GetCapabilities() ocs.OrderedMap {
	return ocs.Obj(
		ocs.K("mail", ocs.Obj(
			ocs.K("enabled", true),
		)),
	)
}
