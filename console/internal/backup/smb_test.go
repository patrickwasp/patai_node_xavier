package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// TestSMB runs against a real server, PATAI_SMB_TEST=host:port,share,user,password.
func TestSMB(t *testing.T) {
	env := os.Getenv("PATAI_SMB_TEST")
	if env == "" {
		t.Skip("PATAI_SMB_TEST isn't set")
	}
	f := strings.Split(env, ",")
	random := make([]byte, 4)
	rand.Read(random)
	folder := "patai-" + hex.EncodeToString(random) + `\backups/unit`
	checkTarget(t, &SMB{Host: f[0], Share: f[1], User: f[2], Password: f[3], Folder: folder}, 20<<20+1000)

	ctx := context.Background()
	for _, tc := range []struct {
		s    SMB
		want string
	}{
		{SMB{Host: f[0], Share: f[1], User: f[2], Password: "wrong"}, "the user name or password was refused"},
		{SMB{Host: f[0], Share: f[1], User: "nobody", Password: f[3]}, "the user name or password was refused"},
		{SMB{Host: f[0], Share: "none", User: f[2], Password: f[3]}, "there's no share by that name"},
	} {
		if _, err := tc.s.List(ctx); err == nil || err.Error() != tc.want {
			t.Errorf("%s %s: %v", tc.s.User, tc.s.Share, err)
		}
	}
	// The share's top is a folder too.
	top := &SMB{Host: f[0], Share: f[1], User: f[2], Password: f[3]}
	if list, err := top.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("the top: %+v %v", list, err)
	}
}
