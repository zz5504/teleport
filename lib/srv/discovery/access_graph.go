/*
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package discovery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"time"

	"github.com/gravitational/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"

	"github.com/gravitational/teleport/api/types"
	accessgraphv1alpha "github.com/gravitational/teleport/gen/proto/go/accessgraph/v1alpha"
	"github.com/gravitational/teleport/lib/service/servicecfg"
	"github.com/gravitational/teleport/lib/services"
	aws_sync "github.com/gravitational/teleport/lib/srv/discovery/fetchers/aws-sync"
)

const (
	// batchSize is the maximum number of resources to send in a single
	// request to the access graph service.
	batchSize = 500
)

func (s *Server) reconcileAccessGraph(ctx context.Context, currentTAGResources *aws_sync.Resources, stream accessgraphv1alpha.AccessGraphService_AWSEventsStreamClient, features aws_sync.Features) {
	type fetcherResult struct {
		result *aws_sync.Resources
		err    error
	}

	allFetchers := s.getAllAWSSyncFetchers()

	resultsC := make(chan fetcherResult, len(allFetchers))
	// Use a channel to limit the number of concurrent fetchers.
	tokens := make(chan struct{}, 3)
	for _, fetcher := range allFetchers {
		fetcher := fetcher
		tokens <- struct{}{}
		go func() {
			defer func() {
				<-tokens
			}()
			result, err := fetcher.Poll(ctx, features)
			resultsC <- fetcherResult{result, trace.Wrap(err)}
		}()
	}

	results := make([]*aws_sync.Resources, 0, len(allFetchers))
	errs := make([]error, 0, len(allFetchers))
	// Collect the results from all fetchers.
	// Each fetcher can return an error and a result.
	for i := 0; i < len(allFetchers); i++ {
		fetcherResult := <-resultsC
		if fetcherResult.err != nil {
			errs = append(errs, fetcherResult.err)
		}
		if fetcherResult.result != nil {
			results = append(results, fetcherResult.result)
		}
	}
	// Aggregate all errors into a single error.
	err := trace.NewAggregate(errs...)
	if err != nil {
		s.Log.WithError(err).Error("Error polling TAGs")
	}
	result := aws_sync.MergeResources(results...)
	// Merge all results into a single result
	upsert, toDel := aws_sync.ReconcileResults(currentTAGResources, result)
	err = push(stream, upsert, toDel)
	if err != nil {
		s.Log.WithError(err).Error("Error pushing TAGs")
		return
	}
	// Update the currentTAGResources with the result of the reconciliation.
	*currentTAGResources = *result
}

// getAllAWSSyncFetchers returns all AWS sync fetchers.
func (s *Server) getAllAWSSyncFetchers() []aws_sync.AWSSync {
	allFetchers := make([]aws_sync.AWSSync, 0, len(s.dynamicTAGSyncFetchers))

	s.muDynamicTAGSyncFetchers.RLock()
	for _, fetcherSet := range s.dynamicTAGSyncFetchers {
		allFetchers = append(allFetchers, fetcherSet...)
	}
	s.muDynamicTAGSyncFetchers.RUnlock()

	allFetchers = append(allFetchers, s.staticTAGSyncFetchers...)
	// TODO(tigrato): submit fetchers event
	return allFetchers
}

func pushUpsertInBatches(
	client accessgraphv1alpha.AccessGraphService_AWSEventsStreamClient,
	upsert *accessgraphv1alpha.AWSResourceList,
) error {
	for i := 0; i < len(upsert.Resources); i += batchSize {
		end := i + batchSize
		if end > len(upsert.Resources) {
			end = len(upsert.Resources)
		}
		err := client.Send(
			&accessgraphv1alpha.AWSEventsStreamRequest{
				Operation: &accessgraphv1alpha.AWSEventsStreamRequest_Upsert{
					Upsert: &accessgraphv1alpha.AWSResourceList{
						Resources: upsert.Resources[i:end],
					},
				},
			},
		)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

func pushDeleteInBatches(
	client accessgraphv1alpha.AccessGraphService_AWSEventsStreamClient,
	toDel *accessgraphv1alpha.AWSResourceList,
) error {
	for i := 0; i < len(toDel.Resources); i += batchSize {
		end := i + batchSize
		if end > len(toDel.Resources) {
			end = len(toDel.Resources)
		}
		err := client.Send(
			&accessgraphv1alpha.AWSEventsStreamRequest{
				Operation: &accessgraphv1alpha.AWSEventsStreamRequest_Delete{
					Delete: &accessgraphv1alpha.AWSResourceList{
						Resources: toDel.Resources[i:end],
					},
				},
			},
		)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

func push(
	client accessgraphv1alpha.AccessGraphService_AWSEventsStreamClient,
	upsert *accessgraphv1alpha.AWSResourceList,
	toDel *accessgraphv1alpha.AWSResourceList,
) error {
	err := pushUpsertInBatches(client, upsert)
	if err != nil {
		return trace.Wrap(err)
	}
	err = pushDeleteInBatches(client, toDel)
	if err != nil {
		return trace.Wrap(err)
	}
	err = client.Send(
		&accessgraphv1alpha.AWSEventsStreamRequest{
			Operation: &accessgraphv1alpha.AWSEventsStreamRequest_Sync{},
		},
	)
	return trace.Wrap(err)
}

// NewAccessGraphClient returns a new access graph service client.
func newAccessGraphClient(ctx context.Context, certs []tls.Certificate, config servicecfg.AccessGraphConfig, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	opt, err := grpcCredentials(config, certs)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	conn, err := grpc.DialContext(ctx, config.Addr, append(opts, opt)...)
	return conn, trace.Wrap(err)
}

// errTAGFeatureNotEnabled is returned when the TAG feature is not enabled
// in the cluster features.
var errTAGFeatureNotEnabled = errors.New("TAG feature is not enabled")

// initializeAndWatchAccessGraph creates a new access graph service client and
// watches the connection state. If the connection is closed, it will
// automatically try to reconnect.
func (s *Server) initializeAndWatchAccessGraph(ctx context.Context, reloadCh <-chan struct{}) error {
	const (
		// aws discovery semaphore lock.
		semaphoreName = "access_graph_aws_sync"
		// Configure health check service to monitor access graph service and
		// automatically reconnect if the connection is lost without
		// relying on new events from the auth server to trigger a reconnect.
		serviceConfig = `{
		 "loadBalancingPolicy": "round_robin",
		 "healthCheckConfig": {
			 "serviceName": ""
		 }
	 }`
	)

	clusterFeatures := s.Config.ClusterFeatures()
	if !clusterFeatures.AccessGraph && (clusterFeatures.Policy == nil || !clusterFeatures.Policy.Enabled) {
		return trace.Wrap(errTAGFeatureNotEnabled)
	}

	const (
		semaphoreExpiration = time.Minute
	)
	// AcquireSemaphoreLock will retry until the semaphore is acquired.
	// This prevents multiple discovery services to push AWS resources in parallel.
	// lease must be released to cleanup the resource in auth server.
	lease, err := services.AcquireSemaphoreLock(
		ctx,
		services.SemaphoreLockConfig{
			Service: s.AccessPoint,
			Params: types.AcquireSemaphoreRequest{
				SemaphoreKind: types.KindAccessGraph,
				SemaphoreName: semaphoreName,
				MaxLeases:     1,
				Expires:       s.clock.Now().Add(semaphoreExpiration),
				Holder:        s.Config.ServerID,
			},
			Expiry: semaphoreExpiration,
			Clock:  s.clock,
		},
	)
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		lease.Stop()
		if err := lease.Wait(); err != nil {
			s.Log.WithError(err).Warn("error cleaning up semaphore")
		}
	}()

	config := s.Config.AccessGraphConfig

	accessGraphConn, err := newAccessGraphClient(
		ctx,
		s.ServerCredentials.Certificates,
		config,
		grpc.WithDefaultServiceConfig(serviceConfig),
	)
	if err != nil {
		return trace.Wrap(err)
	}
	// Close the connection when the function returns.
	defer accessGraphConn.Close()
	client := accessgraphv1alpha.NewAccessGraphServiceClient(accessGraphConn)

	stream, err := client.AWSEventsStream(ctx)
	if err != nil {
		s.Log.WithError(err).Error("Failed to get access graph service stream")
		return trace.Wrap(err)
	}
	header, err := stream.Header()
	if err != nil {
		s.Log.WithError(err).Error("Failed to get access graph service stream header")
		return trace.Wrap(err)
	}
	const (
		supportedResourcesKey = "supported-kinds"
	)
	supportedKinds := header.Get(supportedResourcesKey)
	if len(supportedKinds) == 0 {
		return trace.BadParameter("access graph service did not return supported kinds")
	}
	features := aws_sync.BuildFeatures(supportedKinds...)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Start a goroutine to watch the access graph service connection state.
	// If the connection is closed, cancel the context to stop the event watcher
	// before it tries to send any events to the access graph service.
	go func() {
		defer cancel()
		if !accessGraphConn.WaitForStateChange(ctx, connectivity.Ready) {
			s.Log.Info("access graph service connection was closed")
		}
	}()

	currentTAGResources := &aws_sync.Resources{}
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		s.reconcileAccessGraph(ctx, currentTAGResources, stream, features)
		select {
		case <-ctx.Done():
			return trace.Wrap(ctx.Err())
		case <-ticker.C:
		case <-reloadCh:
		}
	}
}

// grpcCredentials returns a grpc.DialOption configured with TLS credentials.
func grpcCredentials(config servicecfg.AccessGraphConfig, certs []tls.Certificate) (grpc.DialOption, error) {
	var pool *x509.CertPool
	if config.CA != "" {
		pool = x509.NewCertPool()
		caBytes, err := os.ReadFile(config.CA)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, trace.BadParameter("failed to append CA certificate to pool")
		}
	}

	tlsConfig := &tls.Config{
		Certificates:       certs,
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: config.Insecure,
		RootCAs:            pool,
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), nil
}

func (s *Server) initAccessGraphWatchers(ctx context.Context, cfg *Config) error {
	fetchers, err := s.accessGraphFetchersFromMatchers(ctx, cfg.Matchers)
	if err != nil {
		s.Log.WithError(err).Error("Error initializing access graph fetchers")
	}
	s.staticTAGSyncFetchers = fetchers

	if cfg.AccessGraphConfig.Enabled {
		go func() {
			reloadCh := s.newDiscoveryConfigChangedSub()
			for {
				// reset the currentTAGResources to force a full sync
				if err := s.initializeAndWatchAccessGraph(ctx, reloadCh); errors.Is(err, errTAGFeatureNotEnabled) {
					s.Log.Warn("Access Graph specified in config, but the license does not include Teleport Policy. Access graph sync will not be enabled.")
					break
				} else if err != nil {
					s.Log.Warnf("Error initializing and watching access graph: %v", err)
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
				}
			}
		}()
	}
	return nil
}

// accessGraphFetchersFromMatchers converts Matchers into a set of AWS Sync Fetchers.
func (s *Server) accessGraphFetchersFromMatchers(ctx context.Context, matchers Matchers) ([]aws_sync.AWSSync, error) {
	var fetchers []aws_sync.AWSSync
	var errs []error
	if matchers.AccessGraph == nil {
		return fetchers, nil
	}

	for _, awsFetcher := range matchers.AccessGraph.AWS {
		var assumeRole *aws_sync.AssumeRole
		if awsFetcher.AssumeRole != nil {
			assumeRole = &aws_sync.AssumeRole{
				RoleARN:    awsFetcher.AssumeRole.RoleARN,
				ExternalID: awsFetcher.AssumeRole.ExternalID,
			}
		}
		fetcher, err := aws_sync.NewAWSFetcher(
			ctx,
			aws_sync.Config{
				CloudClients: s.CloudClients,
				AssumeRole:   assumeRole,
				Regions:      awsFetcher.Regions,
				Integration:  awsFetcher.Integration,
			},
		)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fetchers = append(fetchers, fetcher)
	}

	return fetchers, trace.NewAggregate(errs...)
}
