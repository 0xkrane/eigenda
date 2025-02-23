package apiserver_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Layr-Labs/eigenda/disperser/apiserver"
	"github.com/Layr-Labs/eigenda/disperser/common/blobstore"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"

	pb "github.com/Layr-Labs/eigenda/api/grpc/disperser"
	"github.com/Layr-Labs/eigenda/common"
	"github.com/Layr-Labs/eigenda/common/aws"
	"github.com/Layr-Labs/eigenda/common/aws/dynamodb"
	"github.com/Layr-Labs/eigenda/common/aws/s3"
	"github.com/Layr-Labs/eigenda/common/logging"
	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/core/mock"
	"github.com/Layr-Labs/eigenda/disperser"
	"github.com/Layr-Labs/eigenda/disperser/batcher"
	"github.com/Layr-Labs/eigenda/inabox/deploy"
	"github.com/Layr-Labs/eigenda/pkg/kzg/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fp"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/peer"
)

var (
	queue              disperser.BlobStore
	dispersalServer    *apiserver.DispersalServer
	dockertestPool     *dockertest.Pool
	dockertestResource *dockertest.Resource
	UUID               = uuid.New()
	metadataTableName  = fmt.Sprintf("test-BlobMetadata-%v", UUID)
	bucketTableName    = fmt.Sprintf("test-BucketStore-%v", UUID)

	deployLocalStack bool
	localStackPort   = "4568"
)

func TestMain(m *testing.M) {
	setup(m)
	code := m.Run()
	teardown()
	os.Exit(code)
}

func TestDisperseBlob(t *testing.T) {
	data := make([]byte, 1024)
	_, err := rand.Read(data)
	assert.NoError(t, err)

	status, _, key := disperseBlob(t, dispersalServer, data)
	assert.Equal(t, status, pb.BlobStatus_PROCESSING)
	assert.NotNil(t, key)
}

func TestDisperseBlobWithInvalidQuorum(t *testing.T) {
	data := make([]byte, 1024)
	_, err := rand.Read(data)
	assert.NoError(t, err)

	p := &peer.Peer{
		Addr: &net.TCPAddr{
			IP:   net.ParseIP("0.0.0.0"),
			Port: 51001,
		},
	}
	ctx := peer.NewContext(context.Background(), p)

	_, err = dispersalServer.DisperseBlob(ctx, &pb.DisperseBlobRequest{
		Data: data,
		SecurityParams: []*pb.SecurityParams{
			{
				QuorumId:           2,
				AdversaryThreshold: 80,
				QuorumThreshold:    100,
			},
		},
	})
	assert.ErrorContains(t, err, "invalid request: the quorum_id must be in range [0, 1], but found 2")

	_, err = dispersalServer.DisperseBlob(ctx, &pb.DisperseBlobRequest{
		Data: data,
		SecurityParams: []*pb.SecurityParams{
			{
				QuorumId:           0,
				AdversaryThreshold: 80,
				QuorumThreshold:    100,
			},
			{
				QuorumId:           0,
				AdversaryThreshold: 50,
				QuorumThreshold:    90,
			},
		},
	})
	assert.ErrorContains(t, err, "invalid request: security_params must not contain duplicate quorum_id")
}

func TestGetBlobStatus(t *testing.T) {
	data := make([]byte, 1024)
	_, err := rand.Read(data)
	assert.NoError(t, err)

	status, blobSize, requestID := disperseBlob(t, dispersalServer, data)
	assert.Equal(t, status, pb.BlobStatus_PROCESSING)
	assert.NotNil(t, requestID)

	reply, err := dispersalServer.GetBlobStatus(context.Background(), &pb.BlobStatusRequest{
		RequestId: requestID,
	})
	assert.NoError(t, err)
	assert.Equal(t, reply.GetStatus(), pb.BlobStatus_PROCESSING)

	// simulate blob confirmation
	securityParams := []*core.SecurityParam{
		{
			QuorumID:           0,
			AdversaryThreshold: 80,
			QuorumThreshold:    100,
		},
		{
			QuorumID:           1,
			AdversaryThreshold: 80,
			QuorumThreshold:    100,
		},
	}
	confirmedMetadata := simulateBlobConfirmation(t, requestID, blobSize, securityParams, 0)

	reply, err = dispersalServer.GetBlobStatus(context.Background(), &pb.BlobStatusRequest{
		RequestId: requestID,
	})
	assert.NoError(t, err)
	assert.Equal(t, reply.GetStatus(), pb.BlobStatus_CONFIRMED)
	actualCommitment, err := new(core.Commitment).Deserialize(reply.GetInfo().GetBlobHeader().GetCommitment())
	assert.NoError(t, err)
	assert.Equal(t, actualCommitment, confirmedMetadata.ConfirmationInfo.BlobCommitment.Commitment)
	assert.Equal(t, reply.GetInfo().GetBlobHeader().GetDataLength(), uint32(confirmedMetadata.ConfirmationInfo.BlobCommitment.Length))

	actualBlobQuorumParams := make([]*pb.BlobQuorumParam, len(securityParams))
	quorumNumbers := make([]byte, len(securityParams))
	quorumPercentSigned := make([]byte, len(securityParams))
	quorumIndexes := make([]byte, len(securityParams))
	for i, sp := range securityParams {
		actualBlobQuorumParams[i] = &pb.BlobQuorumParam{
			QuorumNumber:                 uint32(sp.QuorumID),
			AdversaryThresholdPercentage: uint32(sp.AdversaryThreshold),
			QuorumThresholdPercentage:    uint32(sp.QuorumThreshold),
			QuantizationParam:            uint32(batcher.QuantizationFactor),
			EncodedLength:                uint64(confirmedMetadata.ConfirmationInfo.BlobQuorumInfos[i].EncodedBlobLength),
		}
		quorumNumbers[i] = sp.QuorumID
		quorumPercentSigned[i] = confirmedMetadata.ConfirmationInfo.QuorumResults[sp.QuorumID].PercentSigned
		quorumIndexes[i] = byte(i)
	}
	assert.Equal(t, reply.GetInfo().GetBlobHeader().GetBlobQuorumParams(), actualBlobQuorumParams)

	assert.Equal(t, reply.GetInfo().GetBlobVerificationProof().GetBatchId(), confirmedMetadata.ConfirmationInfo.BatchID)
	assert.Equal(t, reply.GetInfo().GetBlobVerificationProof().GetBlobIndex(), confirmedMetadata.ConfirmationInfo.BlobIndex)
	assert.Equal(t, reply.GetInfo().GetBlobVerificationProof().GetBatchMetadata(), &pb.BatchMetadata{
		BatchHeader: &pb.BatchHeader{
			BatchRoot:               confirmedMetadata.ConfirmationInfo.BatchRoot,
			QuorumNumbers:           quorumNumbers,
			QuorumSignedPercentages: quorumPercentSigned,
			ReferenceBlockNumber:    confirmedMetadata.ConfirmationInfo.ReferenceBlockNumber,
		},
		SignatoryRecordHash:     confirmedMetadata.ConfirmationInfo.SignatoryRecordHash[:],
		Fee:                     confirmedMetadata.ConfirmationInfo.Fee,
		ConfirmationBlockNumber: confirmedMetadata.ConfirmationInfo.ConfirmationBlockNumber,
		BatchHeaderHash:         confirmedMetadata.ConfirmationInfo.BatchHeaderHash[:],
	})
	assert.Equal(t, reply.GetInfo().GetBlobVerificationProof().GetInclusionProof(), confirmedMetadata.ConfirmationInfo.BlobInclusionProof)
	assert.Equal(t, reply.GetInfo().GetBlobVerificationProof().GetQuorumIndexes(), quorumIndexes)
}

func TestRetrieveBlob(t *testing.T) {
	// Create random data
	data := make([]byte, 1024)
	_, err := rand.Read(data)
	assert.NoError(t, err)

	// Disperse the random data
	status, blobSize, requestID := disperseBlob(t, dispersalServer, data)
	assert.Equal(t, status, pb.BlobStatus_PROCESSING)
	assert.NotNil(t, requestID)

	reply, err := dispersalServer.GetBlobStatus(context.Background(), &pb.BlobStatusRequest{
		RequestId: requestID,
	})
	assert.NoError(t, err)
	assert.Equal(t, reply.GetStatus(), pb.BlobStatus_PROCESSING)

	// Simulate blob confirmation so that we can retrieve the blob
	securityParams := []*core.SecurityParam{
		{
			QuorumID:           0,
			AdversaryThreshold: 80,
			QuorumThreshold:    100,
		},
		{
			QuorumID:           1,
			AdversaryThreshold: 80,
			QuorumThreshold:    100,
		},
	}
	_ = simulateBlobConfirmation(t, requestID, blobSize, securityParams, 1)

	reply, err = dispersalServer.GetBlobStatus(context.Background(), &pb.BlobStatusRequest{
		RequestId: requestID,
	})
	assert.NoError(t, err)
	assert.Equal(t, reply.GetStatus(), pb.BlobStatus_CONFIRMED)

	// Retrieve the blob and compare it with the original data
	retrieveData, err := retrieveBlob(t, dispersalServer, 1)
	assert.NoError(t, err)

	assert.Equal(t, data, retrieveData)
}

func TestRetrieveBlobFailsWhenBlobNotConfirmed(t *testing.T) {
	// Create random data
	data := make([]byte, 1024)
	_, err := rand.Read(data)
	assert.NoError(t, err)

	// Disperse the random data
	status, _, requestID := disperseBlob(t, dispersalServer, data)
	assert.Equal(t, status, pb.BlobStatus_PROCESSING)
	assert.NotNil(t, requestID)

	reply, err := dispersalServer.GetBlobStatus(context.Background(), &pb.BlobStatusRequest{
		RequestId: requestID,
	})
	assert.NoError(t, err)
	assert.Equal(t, reply.GetStatus(), pb.BlobStatus_PROCESSING)

	// Try to retrieve the blob before it is confirmed
	_, err = retrieveBlob(t, dispersalServer, 2)
	assert.Error(t, err)
}

func TestDisperseBlobWithExceedSizeLimit(t *testing.T) {
	data := make([]byte, 1024*512+10)
	_, err := rand.Read(data)
	assert.NoError(t, err)

	p := &peer.Peer{
		Addr: &net.TCPAddr{
			IP:   net.ParseIP("0.0.0.0"),
			Port: 51001,
		},
	}
	ctx := peer.NewContext(context.Background(), p)
	_, err = dispersalServer.DisperseBlob(ctx, &pb.DisperseBlobRequest{
		Data: data,
		SecurityParams: []*pb.SecurityParams{
			{
				QuorumId:           0,
				AdversaryThreshold: 80,
				QuorumThreshold:    100,
			},
			{
				QuorumId:           1,
				AdversaryThreshold: 80,
				QuorumThreshold:    100,
			},
		},
	})
	assert.NotNil(t, err)
	assert.Equal(t, err.Error(), "blob size cannot exceed 512 KiB")
}

func setup(m *testing.M) {

	deployLocalStack = !(os.Getenv("DEPLOY_LOCALSTACK") == "false")
	if !deployLocalStack {
		localStackPort = os.Getenv("LOCALSTACK_PORT")
	}

	if deployLocalStack {

		var err error
		dockertestPool, dockertestResource, err = deploy.StartDockertestWithLocalstackContainer(localStackPort)
		if err != nil {
			teardown()
			panic("failed to start localstack container")
		}

	}

	err := deploy.DeployResources(dockertestPool, localStackPort, metadataTableName, bucketTableName)
	if err != nil {
		teardown()
		panic("failed to deploy AWS resources")
	}

	dispersalServer = newTestServer(m)
}

func teardown() {
	if deployLocalStack {
		deploy.PurgeDockertestResources(dockertestPool, dockertestResource)
	}
}

func newTestServer(m *testing.M) *apiserver.DispersalServer {
	logger, err := logging.GetLogger(logging.DefaultCLIConfig())
	if err != nil {
		panic("failed to create a new logger")
	}

	bucketName := "test-eigenda-blobstore"
	awsConfig := aws.ClientConfig{
		Region:          "us-east-1",
		AccessKey:       "localstack",
		SecretAccessKey: "localstack",
		EndpointURL:     fmt.Sprintf("http://0.0.0.0:%s", localStackPort),
	}
	s3Client, err := s3.NewClient(context.Background(), awsConfig, logger)
	if err != nil {
		panic("failed to create s3 client")
	}
	dynamoClient, err := dynamodb.NewClient(awsConfig, logger)
	if err != nil {
		panic("failed to create dynamoDB client")
	}
	blobMetadataStore := blobstore.NewBlobMetadataStore(dynamoClient, logger, metadataTableName, time.Hour)

	var ratelimiter common.RateLimiter
	rateConfig := apiserver.RateConfig{
		QuorumRateInfos: map[core.QuorumID]apiserver.QuorumRateInfo{
			0: {
				PerUserUnauthThroughput: 0,
				TotalUnauthThroughput:   0,
			},
		},
		ClientIPHeader: "",
	}

	queue = blobstore.NewSharedStorage(bucketName, s3Client, blobMetadataStore, logger)
	tx := &mock.MockTransactor{}
	tx.On("GetCurrentBlockNumber").Return(uint32(100), nil)
	tx.On("GetQuorumCount").Return(uint16(2), nil)

	return apiserver.NewDispersalServer(disperser.ServerConfig{
		GrpcPort: "51001",
	}, queue, tx, logger, disperser.NewMetrics("9001", logger), ratelimiter, rateConfig)
}

func disperseBlob(t *testing.T, server *apiserver.DispersalServer, data []byte) (pb.BlobStatus, uint, []byte) {
	p := &peer.Peer{
		Addr: &net.TCPAddr{
			IP:   net.ParseIP("0.0.0.0"),
			Port: 51001,
		},
	}
	ctx := peer.NewContext(context.Background(), p)

	reply, err := server.DisperseBlob(ctx, &pb.DisperseBlobRequest{
		Data: data,
		SecurityParams: []*pb.SecurityParams{
			{
				QuorumId:           0,
				AdversaryThreshold: 80,
				QuorumThreshold:    100,
			},
			{
				QuorumId:           1,
				AdversaryThreshold: 80,
				QuorumThreshold:    100,
			},
		},
	})
	assert.NoError(t, err)
	return reply.GetResult(), uint(len(data)), reply.GetRequestId()
}

func retrieveBlob(t *testing.T, server *apiserver.DispersalServer, blobIndex uint32) ([]byte, error) {
	p := &peer.Peer{
		Addr: &net.TCPAddr{
			IP:   net.ParseIP("0.0.0.0"),
			Port: 51001,
		},
	}
	ctx := peer.NewContext(context.Background(), p)

	reply, err := server.RetrieveBlob(ctx, &pb.RetrieveBlobRequest{
		BatchHeaderHash: []byte{1, 2, 3},
		BlobIndex:       blobIndex,
	})
	if err != nil {
		return nil, err
	}

	return reply.GetData(), nil
}

func simulateBlobConfirmation(t *testing.T, requestID []byte, blobSize uint, securityParams []*core.SecurityParam, blobIndex uint32) *disperser.BlobMetadata {
	ctx := context.Background()

	metadataKey, err := disperser.ParseBlobKey(string(requestID))
	assert.NoError(t, err)

	// simulate processing
	err = queue.MarkBlobProcessing(ctx, metadataKey)
	assert.NoError(t, err)

	// simulate blob confirmation
	batchHeaderHash := [32]byte{1, 2, 3}
	requestedAt := uint64(time.Now().Nanosecond())
	var commitX, commitY fp.Element
	_, err = commitX.SetString("21661178944771197726808973281966770251114553549453983978976194544185382599016")
	assert.NoError(t, err)

	_, err = commitY.SetString("9207254729396071334325696286939045899948985698134704137261649190717970615186")
	assert.NoError(t, err)

	commitment := &core.Commitment{
		G1Point: &bn254.G1Point{
			X: commitX,
			Y: commitY,
		},
	}
	dataLength := 32
	batchID := uint32(99)
	batchRoot := []byte("hello")
	referenceBlockNumber := uint32(132)
	confirmationBlockNumber := uint32(150)
	sigRecordHash := [32]byte{0}
	fee := []byte{0}
	inclusionProof := []byte{1, 2, 3, 4, 5}
	encodedBlobLength := 32
	quorumResults := make(map[core.QuorumID]*core.QuorumResult, len(securityParams))
	quorumInfos := make([]*core.BlobQuorumInfo, len(securityParams))
	for i, sp := range securityParams {
		quorumResults[sp.QuorumID] = &core.QuorumResult{
			QuorumID:      sp.QuorumID,
			PercentSigned: 100,
		}
		quorumInfos[i] = &core.BlobQuorumInfo{
			SecurityParam:      *sp,
			QuantizationFactor: batcher.QuantizationFactor,
			EncodedBlobLength:  uint(encodedBlobLength),
		}
	}

	confirmationInfo := &disperser.ConfirmationInfo{
		BatchHeaderHash:      batchHeaderHash,
		BlobIndex:            blobIndex,
		SignatoryRecordHash:  sigRecordHash,
		ReferenceBlockNumber: referenceBlockNumber,
		BatchRoot:            batchRoot,
		BlobInclusionProof:   inclusionProof,
		BlobCommitment: &core.BlobCommitments{
			Commitment: commitment,
			Length:     uint(dataLength),
		},
		BatchID:                 batchID,
		ConfirmationTxnHash:     gethcommon.HexToHash("0x123"),
		ConfirmationBlockNumber: confirmationBlockNumber,
		Fee:                     fee,
		QuorumResults:           quorumResults,
		BlobQuorumInfos:         quorumInfos,
	}
	metadata := &disperser.BlobMetadata{
		BlobHash:     metadataKey.BlobHash,
		MetadataHash: metadataKey.MetadataHash,
		BlobStatus:   disperser.Processing,
		Expiry:       0,
		NumRetries:   0,
		RequestMetadata: &disperser.RequestMetadata{
			BlobRequestHeader: core.BlobRequestHeader{
				SecurityParams: securityParams,
			},
			RequestedAt: requestedAt,
			BlobSize:    blobSize,
		},
	}
	updated, err := queue.MarkBlobConfirmed(ctx, metadata, confirmationInfo)
	assert.NoError(t, err)

	return updated
}
