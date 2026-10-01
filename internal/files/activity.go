package files

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"

	"github.com/PhantomMatthew/nextcloud-go/internal/activity"
)

// File-lifecycle activity (ADR-0104 §9's first production writer): the DAV
// verbs emit one event per successful operation into the OWNER's stream.
// Names inside subject/subject_rich_parameters follow §9's token rule: a
// scheme-1 owner's file name is stored as the NCGOFN1 token under the row's
// own key (byte-identical to ShareSubjectMeta's construction, so the
// notifications precedent and this stream agree) with the "ncgoNameScheme"
// marker naming the sealing key; the OCS render decrypts in the viewer's ctx
// and rebuilds the subject from the rich template. A token-derivation failure
// SKIPS the event — never a plaintext fallback — while every other failure
// (owner lookup, marshal, insert) is Warn-logged and never fails the verb.
//
// Concretions beyond §9's letter, pinned here:
//   - rename carries TWO tokenized names: "file" (new) sealed by the exact
//     "ncgoNameScheme" marker and "oldfile" (pre-move) sealed by the
//     "ncgoNameScheme:oldfile" marker — the §9 rule applied per sealed param.
//   - a copy is recorded as file_created for the destination (upstream
//     parity), exactly one event: the inner Mkdir/Write verbs of copyOne are
//     muted so a folder copy does not fan out per child.
//   - a trash-less Purge delete emits nothing (upstream purge parity);
//     trash-bin purges emit nothing either.
//   - ObjectName carries file.base (the token for scheme-1 owners); the
//     render side substitutes the decrypted "file" name before the wire.

const (
	activityFileCreated  = "file_created"
	activityFileChanged  = "file_changed"
	activityFileDeleted  = "file_deleted"
	activityFileRenamed  = "file_renamed"
	activityFileRestored = "file_restored"
)

const (
	templateActivityCreated  = "{actor} created {file}"
	templateActivityChanged  = "{actor} changed {file}"
	templateActivityDeleted  = "{actor} deleted {file}"
	templateActivityRenamed  = "{actor} renamed {oldfile} to {file}"
	templateActivityRestored = "{actor} restored {file}"
)

// activitySubjectRef matches one {key} reference in a SubjectRich template.
var activitySubjectRef = regexp.MustCompile(`\{([^{}]+)\}`)

// activityRichParam is one NC rich-object parameter (type/id/name), the same
// shape the notifications producer writes.
type activityRichParam struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// activityName is one event's file-name material: for a scheme-0 owner the
// plaintext basename with an empty keyHex; for a scheme-1 owner the NCGOFN1
// name token plus the sealing key's UUID hex (the render side's resolution
// handle).
type activityName struct {
	base   string
	keyHex string
	fileID int64
}

// activityMuteKey suppresses emission within a ctx: Copy's inner verbs run
// muted so the one pinned event is the Copy-level destination file_created.
type activityMuteKey struct{}

func withActivityMuted(ctx context.Context) context.Context {
	return context.WithValue(ctx, activityMuteKey{}, true)
}

func activityMuted(ctx context.Context) bool {
	m, ok := ctx.Value(activityMuteKey{}).(bool)
	return ok && m
}

// activityNameFor resolves the name material for one owner path: the row's
// plaintext basename and ID through the (translating) meta store, and — for a
// scheme-1 owner with the translation wiring — the §9 subject token via the
// same ShareSubjectMeta primitive the notification producer uses. The row
// must still exist (deletes/renames pre-capture). ANY failure is loud to the
// caller: the event is skipped, never a plaintext fallback.
func (d *DAV) activityNameFor(ctx context.Context, ownerID int64, plainPath string) (activityName, error) {
	np, err := NormalizePath(plainPath)
	if err != nil {
		return activityName{}, err
	}
	row, err := d.Meta.GetByPath(ctx, ownerID, np)
	if err != nil {
		return activityName{}, err
	}
	name := activityName{base: row.Name, fileID: row.ID}
	scheme := 0
	if schemer, ok := d.Users.(UserNameSchemer); ok {
		scheme, err = schemer.UserNameScheme(ctx, ownerID)
		if err != nil {
			return activityName{}, err
		}
	}
	if scheme == 0 || d.Names == nil {
		return name, nil
	}
	ctPath, err := d.shareKeyPath(ctx, ownerID, np)
	if err != nil {
		return activityName{}, err
	}
	token, keyHex, err := d.Names.ShareSubjectMeta(ctx, ownerID, ctPath)
	if err != nil {
		return activityName{}, err
	}
	name.base = token
	name.keyHex = keyHex
	return name, nil
}

// captureActivityName is activityNameFor with the emission gates applied:
// disabled (nil store / zero owner), muted (Copy's inner verbs), or a failed
// token derivation all yield ok=false — the caller skips the event.
func (d *DAV) captureActivityName(ctx context.Context, ownerID int64, plainPath string) (name activityName, ok bool) {
	if d.Activity == nil || ownerID == 0 || activityMuted(ctx) {
		return activityName{}, false
	}
	name, err := d.activityNameFor(ctx, ownerID, plainPath)
	if err != nil {
		d.warn(ctx, "files: activity name capture failed", slog.String("path", plainPath), slog.Any("err", err))
		return activityName{}, false
	}
	return name, true
}

// emitFileActivity captures the name for plainPath and emits; a capture
// failure skips the event (never a plaintext fallback). Rename-style events
// with a pre-captured old name call emitActivity directly.
func (d *DAV) emitFileActivity(ctx context.Context, ownerID int64, actorUID, typ, template, plainPath string) {
	name, ok := d.captureActivityName(ctx, ownerID, plainPath)
	if !ok {
		return
	}
	d.emitActivity(ctx, ownerID, actorUID, typ, template, name, nil)
}

// emitActivity writes one file-lifecycle event to the owner's stream.
// file.base/old.base are the pre-rendered names (tokens for scheme-1 owners —
// the stored subject embeds them, exactly like the notification precedent;
// the render side rebuilds from the template). Insert failures are
// Warn-logged, never returned.
func (d *DAV) emitActivity(ctx context.Context, ownerID int64, actorUID, typ, richTemplate string, file activityName, old *activityName) {
	if ownerID == 0 || d.Activity == nil {
		return
	}
	owner, err := d.Users.GetByID(ctx, ownerID)
	if err != nil {
		d.warn(ctx, "files: activity owner lookup failed", slog.Int64("owner", ownerID), slog.Any("err", err))
		return
	}
	if actorUID == "" {
		actorUID = owner.UID
	}
	displayName := actorUID
	if actor, err := d.Users.GetByUID(ctx, actorUID); err == nil && actor.DisplayName != "" {
		displayName = actor.DisplayName
	}
	params := map[string]activityRichParam{
		"actor": {Type: "user", ID: actorUID, Name: displayName},
		"file":  {Type: "file", ID: strconv.FormatInt(file.fileID, 10), Name: file.base},
	}
	if old != nil {
		params["oldfile"] = activityRichParam{Type: "file", ID: strconv.FormatInt(old.fileID, 10), Name: old.base}
	}
	if file.keyHex != "" {
		params["ncgoNameScheme"] = activityRichParam{Type: "ncgo", ID: file.keyHex, Name: "1"}
	}
	if old != nil && old.keyHex != "" {
		params["ncgoNameScheme:oldfile"] = activityRichParam{Type: "ncgo", ID: old.keyHex, Name: "1"}
	}
	rich, err := json.Marshal(params)
	if err != nil {
		d.warn(ctx, "files: activity marshal rich parameters failed", slog.Any("err", err))
		return
	}
	subject := activitySubjectRef.ReplaceAllStringFunc(richTemplate, func(ref string) string {
		key := ref[1 : len(ref)-1]
		if p, ok := params[key]; ok {
			return p.Name
		}
		return ref
	})
	e := &activity.Event{
		UserID:                ownerID,
		ActorUID:              actorUID,
		App:                   "files",
		Type:                  typ,
		Subject:               subject,
		SubjectRich:           richTemplate,
		SubjectRichParameters: string(rich),
		ObjectType:            "files",
		ObjectID:              file.fileID,
		ObjectName:            file.base,
	}
	if err := d.Activity.Insert(ctx, e); err != nil {
		d.warn(ctx, "files: activity insert failed", slog.Int64("owner", ownerID), slog.String("type", typ), slog.Any("err", err))
	}
}
