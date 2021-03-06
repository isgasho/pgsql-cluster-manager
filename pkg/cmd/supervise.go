package cmd

import (
	"context"
	"net"
	"time"

	"google.golang.org/grpc"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	kitlog "github.com/go-kit/kit/log"
	"github.com/gocardless/pgsql-cluster-manager/pkg/etcd"
	"github.com/gocardless/pgsql-cluster-manager/pkg/failover"
	"github.com/gocardless/pgsql-cluster-manager/pkg/pacemaker"
	"github.com/gocardless/pgsql-cluster-manager/pkg/pgbouncer"
	"github.com/gocardless/pgsql-cluster-manager/pkg/streams"
	"github.com/oklog/run"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func NewSuperviseCommand(ctx context.Context) *cobra.Command {
	c := &cobra.Command{
		Use:   "supervise",
		Short: "Supervise a cluster member",
		Long:  "Sync pacemaker state to etcd and expose a failover API",
		RunE: func(_ *cobra.Command, _ []string) error {
			supervise := &SuperviseCommand{
				client:      mustEtcdClient(),
				pgBouncer:   mustPgBouncer(),
				crm:         pacemaker.NewPacemaker(nil),
				bindAddress: viper.GetString("bind-address"),
				StreamOptions: pacemaker.StreamOptions{
					Ctx:       ctx,
					Attribute: "id",
					XPaths: []pacemaker.AliasedXPath{
						pacemaker.AliasXPath(
							viper.GetString("etcd-postgres-master-key"),
							viper.GetString("postgres-master-crm-xpath"),
						),
					},
					PollInterval: viper.GetDuration("pacemaker-poll-interval"),
					GetTimeout:   viper.GetDuration("pacemaker-get-timeout"),
				},
				RetryFoldOptions: streams.RetryFoldOptions{
					Ctx:      ctx,
					Interval: viper.GetDuration("host-key-update-retry-interval"),
					Timeout:  viper.GetDuration("etcd-timeout"),
				},
			}

			return supervise.Run(ctx, logger)
		},
	}

	c.Flags().String("postgres-master-crm-xpath", pacemaker.MasterXPath, "XPath selector into cibadmin that finds current master")
	c.Flags().String("bind-address", ":8080", "Bind API to this address")
	c.Flags().Duration("host-key-update-retry-interval", time.Second, "Interval to retry etcd update of host key")
	c.Flags().Duration("pacemaker-poll-interval", time.Second, "Interval to poll pacemaker for state changes")
	c.Flags().Duration("pacemaker-get-timeout", 500*time.Millisecond, "Timeout for cib query operation")

	viper.BindPFlags(c.Flags())

	return c
}

type SuperviseCommand struct {
	client      *clientv3.Client
	pgBouncer   *pgbouncer.PgBouncer
	crm         *pacemaker.Pacemaker
	bindAddress string
	pacemaker.StreamOptions
	streams.RetryFoldOptions
}

func (c *SuperviseCommand) Run(ctx context.Context, logger kitlog.Logger) error {
	logger = kitlog.With(logger, "role", "supervise")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var g run.Group

	{
		var logger = kitlog.With(logger, "component", "pacemaker.stream")

		kvs, _ := pacemaker.NewStream(logger, c.crm, c.StreamOptions)
		kvs = streams.DedupeFilter(logger, kvs)

		g.Add(
			func() error {
				return streams.RetryFold(
					logger, kvs, c.RetryFoldOptions,
					func(ctx context.Context, kv *mvccpb.KeyValue) error {
						hostKey, nodeID := string(kv.Key), string(kv.Value)

						logger := kitlog.With(logger, "hostKey", hostKey, "nodeID", nodeID)
						logger.Log("event", "node.change")

						// We get updates for the node ID, the numeric value that represents a
						// corosync member node. We first need to resolve this into an IP address,
						// then push it to etcd.
						addr, err := c.crm.ResolveAddress(ctx, nodeID)
						if err != nil {
							logger.Log("event", "node.resolve.error", "error", err)
							return err
						}

						logger.Log("event", "etcd.update", "addr", addr)
						return etcd.CompareAndUpdate(ctx, c.client, hostKey, addr)
					},
				)
			},
			func(error) { cancel() },
		)
	}

	{
		var logger = kitlog.With(logger, "component", "failover.api")

		listen, err := net.Listen("tcp", c.bindAddress)
		if err != nil {
			return errors.Wrap(err, "failed to bind to address")
		}

		server := failover.NewServer(logger, c.pgBouncer, c.crm)
		grpcServer := grpc.NewServer(grpc.UnaryInterceptor(server.LoggingInterceptor))
		failover.RegisterFailoverServer(grpcServer, server)

		g.Add(
			func() error {
				logger.Log("event", "server.listen", "address", c.bindAddress)
				return grpcServer.Serve(listen)
			},
			func(err error) {
				logger.Log("event", "server.shutdown", "error", err)
				grpcServer.GracefulStop()
			},
		)
	}

	if err := g.Run(); err != nil {
		logger.Log("event", "supervise.finish", "error", err)
		return err
	}

	return nil
}
