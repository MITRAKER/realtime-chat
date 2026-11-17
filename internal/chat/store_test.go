// This file is in package chat_test (not chat) because the contract tests
// in storetest import chat, and chat's own tests can't import a package
// that imports chat: that would be an import cycle.
package chat_test

import (
	"testing"

	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
	"github.com/MITRAKER/realtime-chat/internal/chat/storetest"
)

func TestMemoryStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (chat.Store, func(string) auth.User) {
		var id int64
		return chat.NewMemoryStore(500), func(name string) auth.User {
			id++
			return auth.User{ID: id, Username: name}
		}
	})
}
