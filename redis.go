package proxyd

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/redis/go-redis/v9"
)

const redisOperationTimeout = 20 * time.Millisecond

func NewRedisClient(url string, enable_cluster bool) (redis.UniversalClient, error) {
	if enable_cluster {
		log.Info("Using cluster redis client.")
		opts, err := redis.ParseClusterURL(url)
		if err != nil {
			return nil, err
		}
		opts.ContextTimeoutEnabled = true
		// go-redis v9.2.1 does not propagate this option to cluster node clients.
		opts.NewClient = func(nodeOpts *redis.Options) *redis.Client {
			nodeOpts.ContextTimeoutEnabled = true
			return redis.NewClient(nodeOpts)
		}
		return redis.NewClusterClient(opts), nil
	} else {
		log.Info("Using default redis client.")
		opts, err := redis.ParseURL(url)
		if err != nil {
			return nil, err
		}
		opts.ContextTimeoutEnabled = true
		return redis.NewClient(opts), nil
	}
}

func CheckRedisConnection(client redis.UniversalClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisOperationTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return wrapErr(err, "error connecting to redis")
	}

	return nil
}
