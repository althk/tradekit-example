package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/althk/tradekit/go/harness"
	"github.com/althk/tradekit/go/store"
	"github.com/althk/tradekit/go/zerodha"
)

// loginTimeout bounds how long the bot waits for the browser login. Without
// it, a login nobody completes leaves a scheduled run hung indefinitely.
const loginTimeout = 5 * time.Minute

// sessionKey is where the day's access token is kept between runs, in the
// store's key-value state table.
const sessionKey = "kite_session"

// kiteSession is the persisted token. Kite tokens expire overnight, so the
// issue time is what decides whether a stored one is still worth trying.
type kiteSession struct {
	AccessToken string    `json:"access_token"`
	IssuedAt    time.Time `json:"issued_at"`
}

// login makes the client usable for today's session: the stored token if it
// is still fresh and Kite accepts it, otherwise a browser login, persisted
// for the next run.
//
// The token is checked against the broker, not just by date, because a login
// elsewhere (the Kite web app, another bot) invalidates it without changing
// its age. The browser flow itself — the callback server on
// KITE_REDIRECT_URL, the request-token exchange, the retry page when Kite
// reports a refused login — is harness.BrowserLogin; the adapter satisfies
// ports.BrowserLogin, so this function never sees a request token.
func login(ctx context.Context, client *zerodha.Client, db *store.DB, cfg config) error {
	var saved kiteSession
	if err := db.GetState(ctx, sessionKey, &saved); err == nil && saved.AccessToken != "" {
		client.SetAccessToken(saved.AccessToken, saved.IssuedAt)
		if client.TokenFresh(ctx) {
			_, err := client.Account(ctx)
			if err == nil {
				slog.Info("reusing Kite session", "issued_at", saved.IssuedAt.Format(time.RFC3339))
				return nil
			}
			if !errors.Is(err, zerodha.ErrTokenExpired) {
				return fmt.Errorf("checking stored Kite session: %w", err)
			}
			slog.Info("stored Kite session is no longer valid; logging in again")
		}
	} else if err != nil && !errors.Is(err, store.ErrStateNotFound) {
		return fmt.Errorf("reading stored Kite session: %w", err)
	}

	loginCtx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	token, err := harness.BrowserLogin(loginCtx, client, harness.Callback{RedirectURL: cfg.RedirectURL})
	if err != nil {
		return fmt.Errorf("kite login: %w", err)
	}
	if err := db.SetState(ctx, sessionKey, kiteSession{AccessToken: token, IssuedAt: time.Now()}); err != nil {
		// Not fatal: the client already holds the token. The next run just
		// needs the browser again.
		slog.Warn("could not persist the Kite session; the next run will need a fresh login", "err", err)
	}
	return nil
}
