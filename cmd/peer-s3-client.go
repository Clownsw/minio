// Copyright (c) 2015-2023 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio/internal/grid"
	xhttp "github.com/minio/minio/internal/http"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/minio/internal/rest"
	"github.com/minio/pkg/v2/sync/errgroup"
	"golang.org/x/exp/slices"
)

var errPeerOffline = errors.New("peer is offline")

type peerS3Client interface {
	ListBuckets(ctx context.Context, opts BucketOptions) ([]BucketInfo, error)
	HealBucket(ctx context.Context, bucket string, opts madmin.HealOpts) (madmin.HealResultItem, error)
	GetBucketInfo(ctx context.Context, bucket string, opts BucketOptions) (BucketInfo, error)
	MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error
	DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error

	GetHost() string
	SetPools([]int)
	GetPools() []int
}

type localPeerS3Client struct {
	node  Node
	pools []int
}

func (l *localPeerS3Client) GetHost() string {
	return l.node.Host
}

func (l *localPeerS3Client) SetPools(p []int) {
	l.pools = make([]int, len(p))
	copy(l.pools, p)
}

func (l localPeerS3Client) GetPools() []int {
	return l.pools
}

func (l localPeerS3Client) ListBuckets(ctx context.Context, opts BucketOptions) ([]BucketInfo, error) {
	return listBucketsLocal(ctx, opts)
}

func (l localPeerS3Client) HealBucket(ctx context.Context, bucket string, opts madmin.HealOpts) (madmin.HealResultItem, error) {
	return healBucketLocal(ctx, bucket, opts)
}

func (l localPeerS3Client) GetBucketInfo(ctx context.Context, bucket string, opts BucketOptions) (BucketInfo, error) {
	return getBucketInfoLocal(ctx, bucket, opts)
}

func (l localPeerS3Client) MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error {
	return makeBucketLocal(ctx, bucket, opts)
}

func (l localPeerS3Client) DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error {
	return deleteBucketLocal(ctx, bucket, opts)
}

// client to talk to peer Nodes.
type remotePeerS3Client struct {
	node       Node
	pools      []int
	restClient *rest.Client

	// Function that returns the grid connection for this peer when initialized.
	// Will return nil if the grid connection is not initialized yet.
	gridConn func() *grid.Connection
}

// Wrapper to restClient.Call to handle network errors, in case of network error the connection is marked disconnected
// permanently. The only way to restore the connection is at the xl-sets layer by xlsets.monitorAndConnectEndpoints()
// after verifying format.json
func (client *remotePeerS3Client) call(method string, values url.Values, body io.Reader, length int64) (respBody io.ReadCloser, err error) {
	return client.callWithContext(GlobalContext, method, values, body, length)
}

// Wrapper to restClient.Call to handle network errors, in case of network error the connection is marked disconnected
// permanently. The only way to restore the connection is at the xl-sets layer by xlsets.monitorAndConnectEndpoints()
// after verifying format.json
func (client *remotePeerS3Client) callWithContext(ctx context.Context, method string, values url.Values, body io.Reader, length int64) (respBody io.ReadCloser, err error) {
	if values == nil {
		values = make(url.Values)
	}

	respBody, err = client.restClient.Call(ctx, method, values, body, length)
	if err == nil {
		return respBody, nil
	}

	err = toStorageErr(err)
	return nil, err
}

// S3PeerSys - S3 peer call system.
type S3PeerSys struct {
	peerClients []peerS3Client // Excludes self
	poolsCount  int
}

// NewS3PeerSys - creates new S3 peer calls.
func NewS3PeerSys(endpoints EndpointServerPools) *S3PeerSys {
	return &S3PeerSys{
		peerClients: newPeerS3Clients(endpoints),
		poolsCount:  len(endpoints),
	}
}

// HealBucket - heals buckets at node level
func (sys *S3PeerSys) HealBucket(ctx context.Context, bucket string, opts madmin.HealOpts) (madmin.HealResultItem, error) {
	g := errgroup.WithNErrs(len(sys.peerClients))

	for idx, client := range sys.peerClients {
		idx := idx
		client := client
		g.Go(func() error {
			if client == nil {
				return errPeerOffline
			}
			_, err := client.GetBucketInfo(ctx, bucket, BucketOptions{})
			return err
		}, idx)
	}

	errs := g.Wait()

	var poolErrs []error
	for poolIdx := 0; poolIdx < sys.poolsCount; poolIdx++ {
		perPoolErrs := make([]error, 0, len(sys.peerClients))
		for i, client := range sys.peerClients {
			if slices.Contains(client.GetPools(), poolIdx) {
				perPoolErrs = append(perPoolErrs, errs[i])
			}
		}
		quorum := len(perPoolErrs) / 2
		poolErrs = append(poolErrs, reduceWriteQuorumErrs(ctx, perPoolErrs, bucketOpIgnoredErrs, quorum))
	}

	opts.Remove = isAllBucketsNotFound(poolErrs)
	opts.Recreate = !opts.Remove

	g = errgroup.WithNErrs(len(sys.peerClients))
	healBucketResults := make([]madmin.HealResultItem, len(sys.peerClients))
	for idx, client := range sys.peerClients {
		idx := idx
		client := client
		g.Go(func() error {
			if client == nil {
				return errPeerOffline
			}
			res, err := client.HealBucket(ctx, bucket, opts)
			if err != nil {
				return err
			}
			healBucketResults[idx] = res
			return nil
		}, idx)
	}

	errs = g.Wait()

	for poolIdx := 0; poolIdx < sys.poolsCount; poolIdx++ {
		perPoolErrs := make([]error, 0, len(sys.peerClients))
		for i, client := range sys.peerClients {
			if slices.Contains(client.GetPools(), poolIdx) {
				perPoolErrs = append(perPoolErrs, errs[i])
			}
		}
		quorum := len(perPoolErrs) / 2
		if poolErr := reduceWriteQuorumErrs(ctx, perPoolErrs, bucketOpIgnoredErrs, quorum); poolErr != nil {
			return madmin.HealResultItem{}, poolErr
		}
	}

	for i, err := range errs {
		if err == nil {
			return healBucketResults[i], nil
		}
	}

	return madmin.HealResultItem{}, toObjectErr(errVolumeNotFound, bucket)
}

// ListBuckets lists buckets across all nodes and returns a consistent view:
//   - Return an error when a pool cannot return N/2+1 valid bucket information
//   - For each pool, check if the bucket exists in N/2+1 nodes before including it in the final result
func (sys *S3PeerSys) ListBuckets(ctx context.Context, opts BucketOptions) ([]BucketInfo, error) {
	g := errgroup.WithNErrs(len(sys.peerClients))

	nodeBuckets := make([][]BucketInfo, len(sys.peerClients))

	for idx, client := range sys.peerClients {
		idx := idx
		client := client
		g.Go(func() error {
			if client == nil {
				return errPeerOffline
			}
			localBuckets, err := client.ListBuckets(ctx, opts)
			if err != nil {
				return err
			}
			nodeBuckets[idx] = localBuckets
			return nil
		}, idx)
	}

	errs := g.Wait()

	// The list of buckets in a map to avoid duplication
	resultMap := make(map[string]BucketInfo)

	for poolIdx := 0; poolIdx < sys.poolsCount; poolIdx++ {
		perPoolErrs := make([]error, 0, len(sys.peerClients))
		for i, client := range sys.peerClients {
			if slices.Contains(client.GetPools(), poolIdx) {
				perPoolErrs = append(perPoolErrs, errs[i])
			}
		}
		quorum := len(perPoolErrs) / 2
		if poolErr := reduceWriteQuorumErrs(ctx, perPoolErrs, bucketOpIgnoredErrs, quorum); poolErr != nil {
			return nil, poolErr
		}

		bucketsMap := make(map[string]int)
		for idx, buckets := range nodeBuckets {
			if buckets == nil {
				continue
			}
			if !slices.Contains(sys.peerClients[idx].GetPools(), poolIdx) {
				continue
			}
			for _, bi := range buckets {
				_, ok := resultMap[bi.Name]
				if ok {
					// Skip it, this bucket is found in another pool
					continue
				}
				bucketsMap[bi.Name]++
				if bucketsMap[bi.Name] >= quorum {
					resultMap[bi.Name] = bi
				}
			}
		}
		// loop through buckets and see if some with lost quorum
		// these could be stale buckets lying around, queue a heal
		// of such a bucket. This is needed here as we identify such
		// buckets here while listing buckets. As part of regular
		// globalBucketMetadataSys.Init() call would get a valid
		// buckets only and not the quourum lost ones like this, so
		// explicit call
		for bktName, count := range bucketsMap {
			if count < quorum {
				// Queue a bucket heal task
				globalMRFState.addPartialOp(partialOperation{
					bucket: bktName,
					queued: time.Now(),
				})
			}
		}
	}

	result := make([]BucketInfo, 0, len(resultMap))
	for _, bi := range resultMap {
		result = append(result, bi)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

// GetBucketInfo returns bucket stat info about bucket on disk across all peers
func (sys *S3PeerSys) GetBucketInfo(ctx context.Context, bucket string, opts BucketOptions) (binfo BucketInfo, err error) {
	g := errgroup.WithNErrs(len(sys.peerClients))

	bucketInfos := make([]BucketInfo, len(sys.peerClients))
	for idx, client := range sys.peerClients {
		idx := idx
		client := client
		g.Go(func() error {
			if client == nil {
				return errPeerOffline
			}
			bucketInfo, err := client.GetBucketInfo(ctx, bucket, opts)
			if err != nil {
				return err
			}
			bucketInfos[idx] = bucketInfo
			return nil
		}, idx)
	}

	errs := g.Wait()

	for poolIdx := 0; poolIdx < sys.poolsCount; poolIdx++ {
		perPoolErrs := make([]error, 0, len(sys.peerClients))
		for i, client := range sys.peerClients {
			if slices.Contains(client.GetPools(), poolIdx) {
				perPoolErrs = append(perPoolErrs, errs[i])
			}
		}
		quorum := len(perPoolErrs) / 2
		if poolErr := reduceWriteQuorumErrs(ctx, perPoolErrs, bucketOpIgnoredErrs, quorum); poolErr != nil {
			return BucketInfo{}, poolErr
		}
	}

	for i, err := range errs {
		if err == nil {
			return bucketInfos[i], nil
		}
	}

	return BucketInfo{}, toObjectErr(errVolumeNotFound, bucket)
}

func (client *remotePeerS3Client) ListBuckets(ctx context.Context, opts BucketOptions) ([]BucketInfo, error) {
	v := url.Values{}
	v.Set(peerS3BucketDeleted, strconv.FormatBool(opts.Deleted))

	respBody, err := client.call(peerS3MethodListBuckets, v, nil, -1)
	if err != nil {
		return nil, err
	}
	defer xhttp.DrainBody(respBody)

	var buckets []BucketInfo
	err = gob.NewDecoder(respBody).Decode(&buckets)
	return buckets, err
}

func (client *remotePeerS3Client) HealBucket(ctx context.Context, bucket string, opts madmin.HealOpts) (madmin.HealResultItem, error) {
	conn := client.gridConn()
	if conn == nil {
		return madmin.HealResultItem{}, nil
	}

	mss := grid.NewMSSWith(map[string]string{
		peerS3Bucket:        bucket,
		peerS3BucketDeleted: strconv.FormatBool(opts.Remove),
	})

	_, err := healBucketHandler.Call(ctx, conn, mss)

	// Initialize heal result info
	return madmin.HealResultItem{
		Type:     madmin.HealItemBucket,
		Bucket:   bucket,
		SetCount: -1, // explicitly set an invalid value -1, for bucket heal scenario
	}, toStorageErr(err)
}

// GetBucketInfo returns bucket stat info from a peer
func (client *remotePeerS3Client) GetBucketInfo(ctx context.Context, bucket string, opts BucketOptions) (BucketInfo, error) {
	conn := client.gridConn()
	if conn == nil {
		return BucketInfo{}, nil
	}

	mss := grid.NewMSSWith(map[string]string{
		peerS3Bucket:        bucket,
		peerS3BucketDeleted: strconv.FormatBool(opts.Deleted),
	})

	volInfo, err := headBucketHandler.Call(ctx, conn, mss)
	if err != nil {
		return BucketInfo{}, toStorageErr(err)
	}

	return BucketInfo{
		Name:    volInfo.Name,
		Created: volInfo.Created,
	}, nil
}

// MakeBucket creates bucket across all peers
func (sys *S3PeerSys) MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error {
	g := errgroup.WithNErrs(len(sys.peerClients))
	for idx, client := range sys.peerClients {
		client := client
		g.Go(func() error {
			if client == nil {
				return errPeerOffline
			}
			return client.MakeBucket(ctx, bucket, opts)
		}, idx)
	}
	errs := g.Wait()

	for poolIdx := 0; poolIdx < sys.poolsCount; poolIdx++ {
		perPoolErrs := make([]error, 0, len(sys.peerClients))
		for i, client := range sys.peerClients {
			if slices.Contains(client.GetPools(), poolIdx) {
				perPoolErrs = append(perPoolErrs, errs[i])
			}
		}
		if poolErr := reduceWriteQuorumErrs(ctx, perPoolErrs, bucketOpIgnoredErrs, len(perPoolErrs)/2+1); poolErr != nil {
			return toObjectErr(poolErr, bucket)
		}
	}
	return nil
}

// MakeBucket creates a bucket on a peer
func (client *remotePeerS3Client) MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error {
	conn := client.gridConn()
	if conn == nil {
		return nil
	}

	mss := grid.NewMSSWith(map[string]string{
		peerS3Bucket:            bucket,
		peerS3BucketForceCreate: strconv.FormatBool(opts.ForceCreate),
	})

	_, err := makeBucketHandler.Call(ctx, conn, mss)
	return toStorageErr(err)
}

// DeleteBucket deletes bucket across all peers
func (sys *S3PeerSys) DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error {
	g := errgroup.WithNErrs(len(sys.peerClients))
	for idx, client := range sys.peerClients {
		client := client
		g.Go(func() error {
			if client == nil {
				return errPeerOffline
			}
			return client.DeleteBucket(ctx, bucket, opts)
		}, idx)
	}
	errs := g.Wait()

	for poolIdx := 0; poolIdx < sys.poolsCount; poolIdx++ {
		perPoolErrs := make([]error, 0, len(sys.peerClients))
		for i, client := range sys.peerClients {
			if slices.Contains(client.GetPools(), poolIdx) {
				perPoolErrs = append(perPoolErrs, errs[i])
			}
		}
		poolErr := reduceWriteQuorumErrs(ctx, perPoolErrs, bucketOpIgnoredErrs, len(perPoolErrs)/2+1)
		if poolErr != nil && !errors.Is(poolErr, errVolumeNotFound) {
			if !opts.NoRecreate {
				// re-create successful deletes, since we are return an error.
				sys.MakeBucket(ctx, bucket, MakeBucketOptions{})
			}
			return toObjectErr(poolErr, bucket)
		}
	}
	return nil
}

// DeleteBucket deletes bucket on a peer
func (client *remotePeerS3Client) DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error {
	conn := client.gridConn()
	if conn == nil {
		return nil
	}

	mss := grid.NewMSSWith(map[string]string{
		peerS3Bucket:            bucket,
		peerS3BucketForceDelete: strconv.FormatBool(opts.Force),
	})

	_, err := deleteBucketHandler.Call(ctx, conn, mss)
	return toStorageErr(err)
}

func (client remotePeerS3Client) GetHost() string {
	return client.node.Host
}

func (client remotePeerS3Client) GetPools() []int {
	return client.pools
}

func (client *remotePeerS3Client) SetPools(p []int) {
	client.pools = make([]int, len(p))
	copy(client.pools, p)
}

// newPeerS3Clients creates new peer clients.
func newPeerS3Clients(endpoints EndpointServerPools) (peers []peerS3Client) {
	nodes := endpoints.GetNodes()
	peers = make([]peerS3Client, len(nodes))
	for i, node := range nodes {
		if node.IsLocal {
			peers[i] = &localPeerS3Client{node: node}
		} else {
			peers[i] = newPeerS3Client(node)
		}
		peers[i].SetPools(node.Pools)
	}

	return peers
}

// Returns a peer S3 client.
func newPeerS3Client(node Node) peerS3Client {
	scheme := "http"
	if globalIsTLS {
		scheme = "https"
	}

	serverURL := &url.URL{
		Scheme: scheme,
		Host:   node.Host,
		Path:   peerS3Path,
	}

	restClient := rest.NewClient(serverURL, globalInternodeTransport, newCachedAuthToken())
	// Use a separate client to avoid recursive calls.
	healthClient := rest.NewClient(serverURL, globalInternodeTransport, newCachedAuthToken())
	healthClient.NoMetrics = true

	// Construct a new health function.
	restClient.HealthCheckFn = func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), restClient.HealthCheckTimeout)
		defer cancel()
		respBody, err := healthClient.Call(ctx, peerS3MethodHealth, nil, nil, -1)
		xhttp.DrainBody(respBody)
		return !isNetworkError(err)
	}

	var gridConn atomic.Pointer[grid.Connection]

	return &remotePeerS3Client{
		node: node, restClient: restClient,
		gridConn: func() *grid.Connection {
			// Lazy initialization of grid connection.
			// When we create this peer client, the grid connection is likely not yet initialized.
			if node.GridHost == "" {
				logger.LogOnceIf(context.Background(), fmt.Errorf("gridHost is empty for peer %s", node.Host), node.Host+":gridHost")
				return nil
			}
			gc := gridConn.Load()
			if gc != nil {
				return gc
			}
			gm := globalGrid.Load()
			if gm == nil {
				return nil
			}
			gc = gm.Connection(node.GridHost)
			if gc == nil {
				logger.LogOnceIf(context.Background(), fmt.Errorf("gridHost %s not found for peer %s", node.GridHost, node.Host), node.Host+":gridHost")
				return nil
			}
			gridConn.Store(gc)
			return gc
		},
	}
}
