package main

import (
	"fmt"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/push"
)

// `cloudctl push keygen` mints the VAPID keypair a deployment signs
// notifications with.
//
// A command rather than something the API generates on first start, because the
// PUBLIC half is baked into every subscription a browser has already created:
// PushManager.subscribe binds a subscription to the applicationServerKey it was
// handed, and a notification signed by a different key is refused. A key
// regenerated on restart would silently invalidate every subscription on every
// deploy, and the symptom — notifications quietly stopping for everyone who
// subscribed before the last restart — points nowhere near the cause.
//
// So it is generated once, deliberately, and pasted into configuration where it
// is as durable as the rest of it.
func pushCommand(args []string) error {
	if len(args) == 0 || args[0] != "keygen" {
		return fmt.Errorf("usage: cloudctl push keygen")
	}

	publicKey, privateKey, err := push.GenerateKeys()
	if err != nil {
		return err
	}

	// Printed as the two environment variables rather than as a pair of bare
	// strings, so what to do with them is not a second question. The subject is
	// left as a placeholder because only the operator knows it, and it is
	// required: push services treat it as the abuse contact for this server.
	fmt.Printf(`PC_VAPID_PRIVATE_KEY=%s
PC_VAPID_SUBJECT=mailto:you@example.com

The public key is derived from the private one at startup and served to browsers
at GET /api/v1/push/key; it is not configured separately, so the two halves
cannot drift apart. For reference it is:

  %s

Keep the private key with the rest of your secrets. Replacing it invalidates
every push subscription that already exists, because each one is bound to the
key it was created against - the browsers will simply stop receiving
notifications, and will fall back to polling GET /changes as they always do.
`, privateKey, publicKey)
	return nil
}
