package service

import (
	"regexp"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database/model"
)

var resellerOwnershipTagPattern = regexp.MustCompile(`(?i)\[reseller:[^\]]+\]`)

func ResellerCommentTag(username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}
	return "[reseller:" + username + "]"
}

func UpsertResellerComment(rawComment, username string) string {
	comment := strings.TrimSpace(rawComment)
	tag := ResellerCommentTag(username)
	if tag == "" {
		return comment
	}
	if comment == "" {
		return tag
	}
	if strings.Contains(comment, tag) {
		return comment
	}
	return strings.TrimSpace(comment + " " + tag)
}

func StripResellerComment(rawComment string) string {
	return strings.TrimSpace(resellerOwnershipTagPattern.ReplaceAllString(rawComment, " "))
}

func mergeResellerOwnershipTags(nextComment, previousComment string, previousOwnerUserID, nextOwnerUserID int) string {
	previousTags := resellerOwnershipTagPattern.FindAllString(previousComment, -1)
	nextTags := resellerOwnershipTagPattern.FindAllString(nextComment, -1)
	desiredTags := previousTags
	switch {
	case previousOwnerUserID > 0:
		// The stored owner ID is authoritative; do not accept a different
		// reseller tag from an inbound payload.
	case nextOwnerUserID > 0:
		// A reseller client edit explicitly sets its owner in the controller.
		desiredTags = nextTags
	case len(previousTags) == 0:
		desiredTags = nextTags
	}
	base := StripResellerComment(nextComment)
	if len(desiredTags) == 0 {
		return base
	}
	return strings.TrimSpace(base + " " + strings.Join(desiredTags, " "))
}

func ClientOwnedByReseller(client model.Client, user *model.User) bool {
	if user == nil {
		return false
	}
	if client.OwnerUserID > 0 {
		return client.OwnerUserID == user.Id
	}
	tag := ResellerCommentTag(user.Username)
	return tag != "" && strings.Contains(strings.TrimSpace(client.Comment), tag)
}
