package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
	"time"
)

// randSuffix 给 fixture 标识带随机量：-p 1 挡不住跨轮次残留（AGENTS.md §5.5）。
func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// mkPATUser 建一个测试账号并登记清理（PAT 行随 ON DELETE CASCADE 一起走）。
func mkPATUser(t *testing.T, st *Store, prefix string) *User {
	t.Helper()
	email := strings.ToLower(prefix + "-" + randSuffix(t) + "@pat-store.example.com")
	u, err := st.NewUsers().Create(context.Background(), email, "x", RoleMember)
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u
}

// AC-1 结构断言：表、列集合、token_hash 唯一、user_id 外键 ON DELETE CASCADE。
func TestPATSchema(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	rows, err := st.pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'personal_access_tokens'`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	rows.Close()
	sort.Strings(got)
	want := []string{"created_at", "expires_at", "id", "last_used_at", "name", "prefix",
		"revoked_at", "token_hash", "use_count", "user_id"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("personal_access_tokens 列集合 got=%v want=%v", got, want)
	}

	var uniq bool
	if err := st.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indrelid
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
		  WHERE c.relname = 'personal_access_tokens' AND i.indisunique
		    AND a.attname = 'token_hash' AND i.indnatts = 1)`).Scan(&uniq); err != nil {
		t.Fatal(err)
	}
	if !uniq {
		t.Error("token_hash 上应有唯一索引 got=false want=true")
	}

	var ref, delType string
	if err := st.pool.QueryRow(ctx, `
		SELECT confrelid::regclass::text, confdeltype::text
		FROM pg_constraint
		WHERE conrelid = 'personal_access_tokens'::regclass AND contype = 'f'`).Scan(&ref, &delType); err != nil {
		t.Fatalf("查外键失败: %v", err)
	}
	if ref != "users" || delType != "c" {
		t.Errorf("user_id 外键 got=(%s,%s) want=(users,c=CASCADE)", ref, delType)
	}
}

func TestPATStoreLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	pats := st.NewPATs()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"给定新签发的令牌_断言明文形态与 Lookup 命中且回令牌行 id", func(t *testing.T) {
			u := mkPATUser(t, st, "life")
			plain, id, err := pats.Create(ctx, u.ID, "ci", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(plain, PATPrefix) || len(plain) != len(PATPrefix)+43 {
				t.Fatalf("明文形态 got=%q（len=%d） want=lathe_pat_+43", plain, len(plain))
			}
			got, gotID, err := pats.Lookup(ctx, plain)
			if err != nil || got == nil {
				t.Fatalf("Lookup got=(%v,%v) want=user", got, err)
			}
			if got.ID != u.ID || gotID != id || got.Email != u.Email || got.WebhookSlug == "" {
				t.Errorf("Lookup got=(uid=%d,tid=%d,email=%q,slug=%q) want=(%d,%d,%q,非空)",
					got.ID, gotID, got.Email, got.WebhookSlug, u.ID, id, u.Email)
			}
		}},
		{"给定吊销后的令牌_断言 Lookup 失败且列表不含", func(t *testing.T) {
			u := mkPATUser(t, st, "revoke")
			plain, id, err := pats.Create(ctx, u.ID, "x", nil)
			if err != nil {
				t.Fatal(err)
			}
			ok, err := pats.Revoke(ctx, u.ID, id)
			if err != nil || !ok {
				t.Fatalf("Revoke got=(%v,%v) want=(true,nil)", ok, err)
			}
			if u2, _, err := pats.Lookup(ctx, plain); err == nil || u2 != nil {
				t.Errorf("吊销后 Lookup got=(%v,%v) want=(nil,err)", u2, err)
			}
			list, err := pats.List(ctx, u.ID)
			if err != nil || len(list) != 0 {
				t.Errorf("吊销后 List got=%d 项 err=%v want=0", len(list), err)
			}
		}},
		{"给定别人的令牌 id_断言 Revoke 回 false 且不动它", func(t *testing.T) {
			a := mkPATUser(t, st, "own-a")
			b := mkPATUser(t, st, "own-b")
			plain, id, err := pats.Create(ctx, b.ID, "b", nil)
			if err != nil {
				t.Fatal(err)
			}
			ok, err := pats.Revoke(ctx, a.ID, id)
			if err != nil || ok {
				t.Fatalf("越权 Revoke got=(%v,%v) want=(false,nil)", ok, err)
			}
			if u, _, err := pats.Lookup(ctx, plain); err != nil || u == nil {
				t.Errorf("B 的令牌应仍可用 got=(%v,%v)", u, err)
			}
		}},
		{"给定停用账号_断言 Lookup 失败（SQL 第二道防线）", func(t *testing.T) {
			u := mkPATUser(t, st, "disabled")
			plain, _, err := pats.Create(ctx, u.ID, "x", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.NewUsers().SetDisabled(ctx, u.ID, true); err != nil {
				t.Fatal(err)
			}
			if u2, _, err := pats.Lookup(ctx, plain); err == nil || u2 != nil {
				t.Errorf("停用后 Lookup got=(%v,%v) want=(nil,err)", u2, err)
			}
		}},
		{"给定两把未过期令牌_断言 RevokeUser 吊销全部并回行数 2", func(t *testing.T) {
			u := mkPATUser(t, st, "revuser")
			future := time.Now().Add(time.Hour)
			for i := 0; i < 2; i++ {
				if _, _, err := pats.Create(ctx, u.ID, "k", &future); err != nil {
					t.Fatal(err)
				}
			}
			n, err := pats.RevokeUser(ctx, u.ID)
			if err != nil || n != 2 {
				t.Fatalf("RevokeUser got=(%d,%v) want=(2,nil)", n, err)
			}
			cnt, err := pats.CountActive(ctx, u.ID)
			if err != nil || cnt != 0 {
				t.Errorf("CountActive got=(%d,%v) want=0", cnt, err)
			}
		}},
		{"给定吊销满 30 天与刚吊销的行_断言 GC 只清前者", func(t *testing.T) {
			u := mkPATUser(t, st, "gc")
			_, oldID, err := pats.Create(ctx, u.ID, "old", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, newID, err := pats.Create(ctx, u.ID, "new", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, expID, err := pats.Create(ctx, u.ID, "exp", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `UPDATE personal_access_tokens SET revoked_at = now() - interval '31 days' WHERE id = $1`, oldID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `UPDATE personal_access_tokens SET revoked_at = now() WHERE id = $1`, newID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `UPDATE personal_access_tokens SET expires_at = now() - interval '31 days' WHERE id = $1`, expID); err != nil {
				t.Fatal(err)
			}
			if _, err := pats.GC(ctx); err != nil {
				t.Fatal(err)
			}
			var left []int64
			rows, err := st.pool.Query(ctx, `SELECT id FROM personal_access_tokens WHERE user_id = $1 ORDER BY id`, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id int64
				_ = rows.Scan(&id)
				left = append(left, id)
			}
			rows.Close()
			if len(left) != 1 || left[0] != newID {
				t.Errorf("GC 后剩余 got=%v want=[%d]", left, newID)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}
