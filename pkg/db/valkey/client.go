// Package valkey provides a thin wrapper around valkey-go for the
// redis-compatible key/value backends the orchestrator uses.
package valkey

import (
	"context"
	"fmt"

	"code-agent/pkg/logging"

	vk "github.com/valkey-io/valkey-go"
)

type Config struct {
	Host string `env:"REDIS_HOST, default=localhost"`
	Port int    `env:"REDIS_PORT, default=6379"`
	User string `env:"REDIS_USER, default=default"`
	Pass string `env:"REDIS_PASS"`
	Db   int    `env:"REDIS_DB, default=0"`
}

type Valkey struct {
	Client vk.Client
}

func New(ctx context.Context, config Config) (*Valkey, error) {
	l := logging.Get(ctx)
	addr := fmt.Sprintf("%s:%d", config.Host, config.Port)

	l.Info().Str("addr", addr).Str("user", config.User).Int("db", config.Db).Msg("valkey: configuring client")

	client, err := vk.NewClient(vk.ClientOption{
		InitAddress: []string{addr},
		Username:    config.User,
		Password:    config.Pass,
		SelectDB:    config.Db,
	})
	if err != nil {
		l.Err(err).Str("addr", addr).Msg("valkey: failed to create client")
		return nil, err
	}

	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		l.Err(err).Str("addr", addr).Msg("valkey: failed to connect")
		client.Close()
		return nil, err
	}
	l.Info().Str("addr", addr).Msg("valkey: connected")

	return &Valkey{Client: client}, nil
}

func (v *Valkey) Close() {
	v.Client.Close()
}
