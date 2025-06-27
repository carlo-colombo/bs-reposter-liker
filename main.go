package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/xrpc"
	"golang.org/x/exp/slices"
)

const (
	BlueskyPDS = "https://bsky.social" // The default PDS for Bluesky
)

func main() {
	// Initialize slog logger. Using a TextHandler for console readability.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{}))
	slog.SetDefault(logger)

	// --- Define command-line flags ---
	dryRun := flag.Bool("dry-run", false, "Enable dry run mode (no actual likes or reposts will be performed)")
	flag.Parse() // Parse the command-line flags

	// --- Configuration: Read from Environment Variables ---
	yourHandle := os.Getenv("BLUESKY_HANDLE")
	yourPassword := os.Getenv("BLUESKY_PASSWORD")
	targetUserDID := os.Getenv("TARGET_USER_DID")

	// Validate environment variables
	if yourHandle == "" {
		slog.Error("BLUESKY_HANDLE environment variable not set. Exiting.", "error", "missing_env_var")
		os.Exit(1)
	}
	if yourPassword == "" {
		slog.Error("BLUESKY_PASSWORD environment variable not set. Please use an app password. Exiting.", "error", "missing_env_var")
		os.Exit(1)
	}
	if targetUserDID == "" {
		slog.Error("TARGET_USER_DID environment variable not set. Exiting.", "error", "missing_env_var")
		os.Exit(1)
	}

	slog.Info("Starting Bluesky Auto Reposter and Liker - Stateless Mode",
		"yourHandle", yourHandle,
		"targetUserDID", targetUserDID,
		"dryRun", *dryRun, // Use the value from the flag
	)

	if *dryRun {
		slog.Info("DRY RUN MODE IS ACTIVE. No actual likes or reposts will be performed.")
	} else {
		slog.Info("LIVE RUN MODE IS ACTIVE. Likes and reposts will be performed.")
	}

	// Create a new XRPC client
	ctx := context.Background()

	xrpcc, session, err := AuthenticateAndInit(ctx, yourHandle, yourPassword)
	if err != nil {
		slog.Error("Authentication failed", "error", err)
		os.Exit(1)
	}
	slog.Info("Successfully authenticated",
		"handle", session.Handle,
		"did", session.Did,
	)

	slog.Info("Fetching all posts from target user to find the oldest eligible post...")
	allTargetUserPosts := CollectAllTargetUserPosts(ctx, xrpcc, targetUserDID)
	slog.Info("Finished collecting target user's posts", "totalPostsCollected", len(allTargetUserPosts))

	slices.Reverse(allTargetUserPosts)
	slog.Info("Posts reordered from oldest to newest.")

	post := FindOldestEligiblePost(allTargetUserPosts)
	actionPerformed := false
	if post != nil {
		actionPerformed = ProcessPostActions(ctx, xrpcc, post, *dryRun)
	}

	if !actionPerformed {
		slog.Info("No un-actioned posts found from the target user's collected feed.")
	}

	slog.Info("Program finished.")
}

// AuthenticateAndInit authenticates with Bluesky and returns an authenticated xrpc.Client and session info.
func AuthenticateAndInit(ctx context.Context, handle, password string) (*xrpc.Client, *atproto.ServerCreateSession_Output, error) {
	xrpcc := &xrpc.Client{Host: BlueskyPDS}
	session, err := atproto.ServerCreateSession(ctx, xrpcc, &atproto.ServerCreateSession_Input{
		Identifier: handle,
		Password:   password,
	})
	if err != nil {
		return nil, nil, err
	}
	xrpcc.Auth = &xrpc.AuthInfo{
		AccessJwt:  session.AccessJwt,
		RefreshJwt: session.RefreshJwt,
		Did:        session.Did,
		Handle:     session.Handle,
	}
	return xrpcc, session, nil
}

// CollectAllTargetUserPosts fetches all posts from the target user, stopping at the first fully actioned post.
func CollectAllTargetUserPosts(ctx context.Context, xrpcc *xrpc.Client, targetUserDID string) []*bsky.FeedDefs_PostView {
	var allTargetUserPosts []*bsky.FeedDefs_PostView
	cursor := ""

feedCollect:
	for {
		slog.Info("Fetching author feed for target user", "targetUserDID", targetUserDID, "cursor", cursor)
		feed, err := bsky.FeedGetAuthorFeed(ctx, xrpcc, targetUserDID, cursor, "", false, 10)
		if err != nil {
			slog.Error("Failed to get author feed while collecting all posts",
				"targetUserDID", targetUserDID,
				"error", err,
			)
			break
		}
		if len(feed.Feed) == 0 {
			slog.Info("No more posts to fetch from target user.")
			break
		}
		for _, item := range feed.Feed {
			slog.Info("Processing feed item", "postUri", item.Post.Uri, "t", item.Post.IndexedAt)
			post := item.Post
			if post.Author.Did == targetUserDID {
				alreadyLiked := post.Viewer != nil && post.Viewer.Like != nil
				alreadyReposted := post.Viewer != nil && post.Viewer.Repost != nil
				if alreadyLiked && alreadyReposted {
					break feedCollect
				}
				allTargetUserPosts = append(allTargetUserPosts, post)
			} else {
				slog.Debug("Skipping feed item, not directly authored by target user",
					"postUri", post.Uri,
					"authorDid", post.Author.Did,
					"targetUserDID", targetUserDID,
				)
			}
		}
		slog.Info("Cursor for next page", "cursor", *feed.Cursor)
		if feed.Cursor != nil && *feed.Cursor != "" {
			cursor = *feed.Cursor
			time.Sleep(1 * time.Second)
		} else {
			break
		}
	}
	return allTargetUserPosts
}

// LikePost performs the like action for a given post.
// It takes an additional isDryRun boolean to determine if the action should be skipped.
func LikePost(ctx context.Context, xrpcc *xrpc.Client, uri, cid string, isDryRun bool) error {
	if isDryRun {
		slog.Info("DRY RUN: Would have liked post", "postUri", uri)
		return nil
	}

	record := &bsky.FeedLike{
		Subject: &atproto.RepoStrongRef{
			Cid: cid,
			Uri: uri,
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	_, err := atproto.RepoCreateRecord(ctx, xrpcc, &atproto.RepoCreateRecord_Input{
		Repo:       xrpcc.Auth.Did,
		Collection: "app.bsky.feed.like",
		Record:     &util.LexiconTypeDecoder{Val: record},
	})
	if err != nil {
		return fmt.Errorf("failed to like post URI %s: %w", uri, err)
	}
	slog.Info("Successfully liked post", "postUri", uri)
	return nil
}

// RepostPost performs the repost action for a given post.
// It takes an additional isDryRun boolean to determine if the action should be skipped.
func RepostPost(ctx context.Context, xrpcc *xrpc.Client, uri, cid string, isDryRun bool) error {
	if isDryRun {
		slog.Info("DRY RUN: Would have reposted post", "postUri", uri)
		return nil
	}

	record := &bsky.FeedRepost{
		Subject: &atproto.RepoStrongRef{
			Cid: cid,
			Uri: uri,
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	_, err := atproto.RepoCreateRecord(ctx, xrpcc, &atproto.RepoCreateRecord_Input{
		Repo:       xrpcc.Auth.Did,
		Collection: "app.bsky.feed.repost",
		Record:     &util.LexiconTypeDecoder{Val: record},
	})
	if err != nil {
		return fmt.Errorf("failed to repost post URI %s: %w", uri, err)
	}
	slog.Info("Successfully reposted post", "postUri", uri)
	return nil
}

// ProcessPostActions likes and/or reposts the given post if needed.
func ProcessPostActions(ctx context.Context, xrpcc *xrpc.Client, post *bsky.FeedDefs_PostView, dryRun bool) bool {
	alreadyLiked := post.Viewer != nil && post.Viewer.Like != nil
	alreadyReposted := post.Viewer != nil && post.Viewer.Repost != nil

	slog.Info("Found oldest eligible post to action",
		"postUri", post.Uri,
		"authorDisplayName", post.Author.DisplayName,
		"alreadyLiked", alreadyLiked,
		"alreadyReposted", alreadyReposted,
	)

	if !alreadyLiked {
		err := LikePost(ctx, xrpcc, post.Uri, post.Cid, dryRun)
		if err != nil {
			slog.Error("Error liking post", "postUri", post.Uri, "error", err)
		}
	} else {
		slog.Debug("Post already liked, skipping like action", "postUri", post.Uri)
	}

	if !alreadyReposted {
		err := RepostPost(ctx, xrpcc, post.Uri, post.Cid, dryRun)
		if err != nil {
			slog.Error("Error reposting post", "postUri", post.Uri, "error", err)
		}
	} else {
		slog.Debug("Post already reposted, skipping repost action", "postUri", post.Uri)
	}

	slog.Info("Actioned one oldest eligible post. Exiting program.", "postUri", post.Uri)
	return true
}

// FindOldestEligiblePost returns the first eligible post from the list, or nil if none.
func FindOldestEligiblePost(posts []*bsky.FeedDefs_PostView) *bsky.FeedDefs_PostView {
	for _, post := range posts {
		alreadyLiked := post.Viewer != nil && post.Viewer.Like != nil
		alreadyReposted := post.Viewer != nil && post.Viewer.Repost != nil
		if !alreadyLiked || !alreadyReposted {
			return post
		}
	}
	return nil
}
