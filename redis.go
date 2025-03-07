package yiigo

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/shenghui0779/vitess_pool"
	"go.uber.org/zap"
)

// RedisConn redis connection resource
type RedisConn struct {
	redis.Conn
}

// Close closes the connection resource
func (rc *RedisConn) Close() {
	if err := rc.Conn.Close(); err != nil {
		logger.Error("yiigo: redis conn closed error", zap.Error(err))
	}
}

type redisSetting struct {
	address      string
	password     string
	database     int
	connTimeout  time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration
	pool         *poolSetting
}

// RedisOption configures how we set up the redis.
type RedisOption func(s *redisSetting)

// WithRedisDatabase specifies the database for redis.
func WithRedisDatabase(db int) RedisOption {
	return func(s *redisSetting) {
		s.database = db
	}
}

// WithRedisConnTimeout specifies the `ConnectTimeout` for redis.
func WithRedisConnTimeout(t time.Duration) RedisOption {
	return func(s *redisSetting) {
		s.connTimeout = t
	}
}

// WithRedisReadTimeout specifies the `ReadTimeout` for redis.
func WithRedisReadTimeout(t time.Duration) RedisOption {
	return func(s *redisSetting) {
		s.readTimeout = t
	}
}

// WithRedisWriteTimeout specifies the `WriteTimeout` for redis.
func WithRedisWriteTimeout(t time.Duration) RedisOption {
	return func(s *redisSetting) {
		s.writeTimeout = t
	}
}

// WithRedisPool specifies the pool for redis.
func WithRedisPool(options ...PoolOption) RedisOption {
	return func(s *redisSetting) {
		for _, f := range options {
			f(s.pool)
		}
	}
}

// RedisPool redis pool resource
type RedisPool interface {
	// Get returns a connection resource from the pool.
	// Context with timeout can specify the wait timeout for pool.
	Get(ctx context.Context) (*RedisConn, error)

	// Put returns a connection resource to the pool.
	Put(rc *RedisConn)
}

type redisPoolResource struct {
	config *redisSetting
	pool   *vitess_pool.ResourcePool
	mutex  sync.Mutex
}

func (r *redisPoolResource) dial() (redis.Conn, error) {
	dialOptions := []redis.DialOption{
		redis.DialPassword(r.config.password),
		redis.DialDatabase(r.config.database),
		redis.DialConnectTimeout(r.config.connTimeout),
		redis.DialReadTimeout(r.config.readTimeout),
		redis.DialWriteTimeout(r.config.writeTimeout),
	}

	conn, err := redis.Dial("tcp", r.config.address, dialOptions...)

	return conn, err
}

func (r *redisPoolResource) init() {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.pool != nil && !r.pool.IsClosed() {
		return
	}

	df := func() (vitess_pool.Resource, error) {
		conn, err := r.dial()

		if err != nil {
			return nil, err
		}

		return &RedisConn{conn}, nil
	}

	r.pool = vitess_pool.NewResourcePool(df, r.config.pool.size, r.config.pool.limit, r.config.pool.idleTimeout, r.config.pool.prefill)
}

func (r *redisPoolResource) Get(ctx context.Context) (*RedisConn, error) {
	if r.pool.IsClosed() {
		r.init()
	}

	resource, err := r.pool.Get(ctx)

	if err != nil {
		return &RedisConn{}, err
	}

	rc := resource.(*RedisConn)

	// If rc is error, close and reconnect
	if rc.Err() != nil {
		conn, err := r.dial()

		if err != nil {
			r.pool.Put(rc)

			return rc, err
		}

		rc.Close()

		return &RedisConn{conn}, nil
	}

	return rc, nil
}

func (r *redisPoolResource) Put(conn *RedisConn) {
	r.pool.Put(conn)
}

var (
	defaultRedis RedisPool
	redisMap     sync.Map
)

func newRedis(address string, options ...RedisOption) RedisPool {
	rp := &redisPoolResource{
		config: &redisSetting{
			address:      address,
			connTimeout:  10 * time.Second,
			readTimeout:  10 * time.Second,
			writeTimeout: 10 * time.Second,
			pool: &poolSetting{
				size:        10,
				idleTimeout: 60 * time.Second,
			},
		},
	}

	for _, f := range options {
		f(rp.config)
	}

	if rp.config.pool.limit < rp.config.pool.size {
		rp.config.pool.limit = rp.config.pool.size
	}

	rp.init()

	return rp
}

func initRedis(name, address string, options ...RedisOption) {
	pool := newRedis(address, options...)

	// verify connection
	conn, err := pool.Get(context.TODO())

	if err != nil {
		logger.Panic("yiigo: redis init error", zap.String("name", name), zap.Error(err))
	}

	if _, err = conn.Do("PING"); err != nil {
		conn.Close()

		logger.Panic("yiigo: redis init error", zap.String("name", name), zap.Error(err))
	}

	pool.Put(conn)

	if name == Default {
		defaultRedis = pool
	}

	redisMap.Store(name, pool)

	logger.Info(fmt.Sprintf("yiigo: redis.%s is OK", name))
}

// Redis returns a redis pool.
func Redis(name ...string) RedisPool {
	if len(name) == 0 || name[0] == Default {
		if defaultRedis == nil {
			logger.Panic(fmt.Sprintf("yiigo: unknown redis.%s (forgotten configure?)", Default))
		}

		return defaultRedis
	}

	v, ok := redisMap.Load(name[0])

	if !ok {
		logger.Panic(fmt.Sprintf("yiigo: unknown redis.%s (forgotten configure?)", name[0]))
	}

	return v.(RedisPool)
}
