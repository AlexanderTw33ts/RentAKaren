package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Restriction mode — a temporary, admin-toggled valve that limits regular members
// so donors keep the full service.
//
// While ON (/restrict on), a "member" (not an admin, not a donor) gets:
//   - memberAlbumsPerDay albums per quota-day (1 PM Dhaka reset, same as quota.go)
//   - memberPlaylistsPerDay playlists per quota-day, none over playlistSleeptimeThreshold
//   - no artist/discography rips at all
//   - unlimited single tracks, music videos and stations (not gated)
// Donors are unlimited (no per-day caps, no heavy-rip caps) and admins are never gated.
//
// While OFF (the default, and what a fresh deploy gets) none of this applies and the
// bot behaves exactly as it did before: only the always-on heavyRipLimit caps remain.
// Donor-only DMs are separate and unchanged (see handleMessage).
//
// The flag lives in telegram-state.json next to adminLock, so it survives restarts
// and rides along in the daily backup.
// =============================================================================

const (
	memberAlbumsPerDay    = 20
	memberPlaylistsPerDay = 2

	// quotaUnlimited is the per-day limit used for donors while restriction mode is on.
	quotaUnlimited = 1 << 30
)

// isRestricted reports whether restriction mode is on.
func (b *TelegramBot) isRestricted() bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.restrictionMode
}

// setRestriction flips restriction mode and persists it.
func (b *TelegramBot) setRestriction(on bool) {
	b.stateMu.Lock()
	b.restrictionMode = on
	b.saveStateLocked()
	b.stateMu.Unlock()
}

// isRestrictedMember reports whether userID is currently subject to the member
// limits: restriction mode is on and they are neither an admin nor a donor. A zero
// userID (headless/CLI) is never restricted. Takes stateMu (via the helpers) — call
// it with no other lock held.
func (b *TelegramBot) isRestrictedMember(userID int64, username string) bool {
	if userID == 0 || b.isAdmin(userID) {
		return false
	}
	return b.isRestricted() && !b.isUserDonor(userID, username)
}

// ripLimit is the per-day limit for a quota kind: heavyRipLimit normally, overridden
// while restriction mode is on (members get the member caps, donors are unlimited).
// Single source of truth for the admission gate (quotaCheckAndCharge), the scheduled
// path's reject message, and the /profile readout.
func (b *TelegramBot) ripLimit(kind string, donor bool) int {
	if b.isRestricted() {
		if donor {
			return quotaUnlimited
		}
		switch kind {
		case "album":
			return memberAlbumsPerDay
		case "playlist":
			return memberPlaylistsPerDay
		}
	}
	return heavyRipLimit(kind, donor)
}

// limitLabel renders a per-day limit for display ("∞" for unlimited).
func limitLabel(limit int) string {
	if limit >= quotaUnlimited {
		return "∞"
	}
	return fmt.Sprint(limit)
}

// admitRestricted charges a per-day slot of kind ("album" | "playlist") for a
// restricted member and tells them when they're out. Everyone else (valve off, donor,
// admin, headless) is admitted with an empty chargeID, which every downstream quota
// call treats as a no-op. The caller owns the returned charge: it must commit or
// refund it on every path.
func (b *TelegramBot) admitRestricted(chatID int64, replyToID int, kind string, userID int64, username string) (string, bool) {
	if !b.isRestrictedMember(userID, username) {
		return "", true
	}
	chargeID, ok := b.quotaCheckAndCharge(userID, username, kind)
	if ok {
		return chargeID, true
	}
	noun, limit := "albums", memberAlbumsPerDay
	if kind == "playlist" {
		noun, limit = "playlists", memberPlaylistsPerDay
	}
	_ = b.sendMessageWithReply(chatID, fmt.Sprintf(
		"You've used your %d %s for today. It resets at 1 PM Dhaka. Failed or cancelled rips don't count, and donors have no limits.",
		limit, noun), nil, replyToID)
	return "", false
}

// denyIfRestrictedMember blocks a feature that members don't get while restriction
// mode is on, telling them why. Returns true when the request was denied.
func (b *TelegramBot) denyIfRestrictedMember(chatID int64, replyToID int, userID int64, username, what string) bool {
	if !b.isRestrictedMember(userID, username) {
		return false
	}
	_ = b.sendMessageWithReply(chatID, fmt.Sprintf("%s are available to donors only while restrictions are on.", what), nil, replyToID)
	return true
}

// trySendCachedAlbumZipQuota is trySendCachedAlbumZip with the member album quota
// applied: a restricted member's cached delivery spends a slot like a fresh rip would.
// Returns true when the request is finished (served from cache, or denied for being
// over the limit); false means the caller should carry on and enqueue the rip.
func (b *TelegramBot) trySendCachedAlbumZipQuota(chatID int64, albumID string, replyToID int, format string, userID int64, username string) bool {
	chargeID, ok := b.admitRestricted(chatID, replyToID, "album", userID, username)
	if !ok {
		return true
	}
	if b.trySendCachedAlbumZip(chatID, albumID, replyToID, format) {
		b.quotaCommit(chargeID, 0)
		return true
	}
	// Not cached: hand the slot back — enqueueAlbumDownload charges its own.
	b.quotaRefund(chargeID, 0)
	return false
}

// handleRestrictCommand implements the admin-only /restrict valve:
//
//	/restrict          show the current mode and limits
//	/restrict on|off   turn member restrictions on or off
func (b *TelegramBot) handleRestrictCommand(chatID int64, args []string, replyToID int) {
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "on":
			b.setRestriction(true)
		case "off":
			b.setRestriction(false)
		default:
			_ = b.sendMessageWithReply(chatID, "Usage: /restrict [on|off]", nil, replyToID)
			return
		}
	}
	if b.isRestricted() {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf(
			"🔒 Restriction mode is ON.\nMembers: %d albums/day, %d playlists/day (max %d tracks), no discographies, unlimited single tracks.\nDonors and admins: unlimited.\nUse /restrict off to free everyone.",
			memberAlbumsPerDay, memberPlaylistsPerDay, playlistSleeptimeThreshold), nil, replyToID)
		return
	}
	_ = b.sendMessageWithReply(chatID, "🔓 Restriction mode is OFF — everyone has the normal limits. Use /restrict on to restrict members.", nil, replyToID)
}
