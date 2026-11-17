// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/emiago/diago"
	"github.com/emiago/diago/examples"
	"github.com/emiago/diago/examples/serving"
	"github.com/emiago/sipgo"
)

// Have receiver running:
// gophone answer -l "127.0.0.1:5090"
//
// Run app:
// go run . sip:uas@127.0.0.1:5090
//
// Run dialer:
// gophone dial sip:bob@127.0.0.1

var bridge *diago.BridgeMix

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	examples.SetupLogger()

	flag.Parse()
	bridge = diago.NewBridgeMix()

	err := start(ctx)
	if err != nil {
		slog.Error("PBX finished with error", "error", err)
	}
}

func start(ctx context.Context) error {
	// Setup our main transaction user
	ua, _ := sipgo.NewUA()
	d := diago.NewDiago(ua)
	return serving.Run(ctx, d, func(ctx context.Context, inDialog *diago.DialogServerSession) {
		BridgeCall(ctx, d, inDialog)

		fmt.Println("Bridge state", bridge.String())
	})
}

func BridgeCall(ctx context.Context, d *diago.Diago, inDialog *diago.DialogServerSession) error {
	inDialog.Trying(ctx)  // Progress -> 100 Trying
	inDialog.Ringing(ctx) // Ringing -> 180 Response

	inCtx := inDialog.Context()

	if err := inDialog.Answer(); err != nil {
		return err
	}
	if err := bridge.AddDialogSession(inDialog); err != nil {
		return err
	}

	<-inCtx.Done()
	return bridge.RemoveDialogSession(inDialog)
}
