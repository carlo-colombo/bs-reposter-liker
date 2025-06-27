// I've updated the Go program to use a command-line flag `--dry-run` instead of an environment variable for enabling dry run mode. This makes it more convenient to toggle the behavior directly when executing the script.

// **Key Changes:**

// * **`flag` package:** The standard `flag` package is imported.
// * **Flag Definition:** A new boolean flag `dryRun` is defined using `flag.BoolVar`.
// * **Flag Parsing:** `flag.Parse()` is called after defining the flag to process command-line arguments.
// * **Environment Variable Removal:** The `os.Getenv("DRY_RUN")` and `strconv.ParseBool` logic for the dry run environment variable has been removed.

// **How to Use (with `--dry-run` Flag):**

// 1.  **Set Environment Variables (Still required for credentials):**
//     * `BLUESKY_HANDLE`: Your Bluesky handle.
//     * `BLUESKY_PASSWORD`: Your Bluesky app password.
//     * `TARGET_USER_HANDLE`: The handle of the user whose posts you want to like/repost.

//     **Example (Linux/macOS):**
//     ```bash
//     export BLUESKY_HANDLE="your-bluesky-handle.bsky.social"
//     export BLUESKY_PASSWORD="your-bluesky-app-password"
//     export TARGET_USER_HANDLE="target-user.bsky.social"
//     ```

// 2.  **Run the program with the flag:**
//     * **Dry Run Mode:**
//         ```bash
//         go run . --dry-run
//         ```
//     * **Live Run Mode:** (Simply omit the `--dry-run` flag)
//         ```bash
//         go run .
//         ```

// This makes the `DRY_RUN` option a direct command-line argument, which is often preferred for script execution.

// ```go
package main

import (
	"context"
	"flag" // New import for command-line flags
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

// Bluesky configuration
const (
	BlueskyPDS = "https://bsky.social" // The default PDS for Bluesky
)

func main() {
	// Initialize slog logger. Using a TextHandler for console readability.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		// Level: slog.LevelDebug, // Uncomment this line to see DEBUG level logs
	}))
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
	xrpcc := &xrpc.Client{Host: BlueskyPDS}

	// Authenticate with Bluesky
	session, err := atproto.ServerCreateSession(ctx, xrpcc, &atproto.ServerCreateSession_Input{
		Identifier: yourHandle,
		Password:   yourPassword,
	})
	if err != nil {
		slog.Error("Authentication failed", "error", err)
		os.Exit(1)
	}
	xrpcc.Auth = &xrpc.AuthInfo{
		AccessJwt:  session.AccessJwt,
		RefreshJwt: session.RefreshJwt,
		Did:        session.Did,
		Handle:     session.Handle,
	}
	slog.Info("Successfully authenticated",
		"handle", session.Handle,
		"did", session.Did,
	)

	// --- Fetch all posts from the target user to find the oldest un-actioned one ---
	var allTargetUserPosts []*bsky.FeedDefs_PostView // Stores posts from newest to oldest initially

	slog.Info("Fetching all posts from target user to find the oldest eligible post...")

	cursor := "" // Start with empty string for the first request

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
			// Only consider posts authored directly by the target user
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

		if feed.Cursor != nil && *feed.Cursor != "" { // Avoid infinite loop with "cursor" as a cursor
			cursor = *feed.Cursor
			time.Sleep(1 * time.Second) // Small delay between page fetches
		} else {
			break
		}
	}

	slog.Info("Finished collecting target user's posts", "totalPostsCollected", len(allTargetUserPosts))

	// Reverse the slice to get posts from oldest to newest
	slices.Reverse(allTargetUserPosts)
	slog.Info("Posts reordered from oldest to newest.")

	actionPerformed := false
	if len(allTargetUserPosts) > 0 {
		post := allTargetUserPosts[0] // The oldest eligible post
		alreadyLiked := post.Viewer != nil && post.Viewer.Like != nil
		alreadyReposted := post.Viewer != nil && post.Viewer.Repost != nil

		slog.Info("Found oldest eligible post to action",
			"postUri", post.Uri,
			"authorDisplayName", post.Author.DisplayName,
			"alreadyLiked", alreadyLiked,
			"alreadyReposted", alreadyReposted,
		)

		// --- Like the post (if not already liked) ---
		if !alreadyLiked {
			err := LikePost(ctx, xrpcc, post.Uri, post.Cid, *dryRun, logger) // Pass dryRun flag
			if err != nil {
				slog.Error("Error liking post",
					"postUri", post.Uri,
					"error", err,
				)
			}
		} else {
			slog.Debug("Post already liked, skipping like action", "postUri", post.Uri)
		}

		// --- Repost the post (if not already reposted) ---
		if !alreadyReposted {
			err := RepostPost(ctx, xrpcc, post.Uri, post.Cid, *dryRun, logger) // Pass dryRun flag
			if err != nil {
				slog.Error("Error reposting post",
					"postUri", post.Uri,
					"error", err,
				)
			}
		} else {
			slog.Debug("Post already reposted, skipping repost action", "postUri", post.Uri)
		}

		actionPerformed = true
		slog.Info("Actioned one oldest eligible post. Exiting program.", "postUri", post.Uri)
	}

	if !actionPerformed {
		slog.Info("No un-actioned posts found from the target user's collected feed.")
	}

	slog.Info("Program finished.")
}

// LikePost performs the like action for a given post.
// It takes an additional isDryRun boolean to determine if the action should be skipped.
func LikePost(ctx context.Context, xrpcc *xrpc.Client, uri, cid string, isDryRun bool, logger *slog.Logger) error {
	if isDryRun {
		logger.Info("DRY RUN: Would have liked post", "postUri", uri)
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
	logger.Info("Successfully liked post", "postUri", uri)
	return nil
}

// RepostPost performs the repost action for a given post.
// It takes an additional isDryRun boolean to determine if the action should be skipped.
func RepostPost(ctx context.Context, xrpcc *xrpc.Client, uri, cid string, isDryRun bool, logger *slog.Logger) error {
	if isDryRun {
		logger.Info("DRY RUN: Would have reposted post", "postUri", uri)
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
	logger.Info("Successfully reposted post", "postUri", uri)
	return nil
}
