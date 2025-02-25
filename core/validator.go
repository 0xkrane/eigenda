package core

import (
	"errors"
)

var (
	ErrChunkLengthMismatch = errors.New("chunk length mismatch")
	ErrInvalidHeader       = errors.New("invalid header")
)

type ChunkValidator interface {
	ValidateBlob(*BlobMessage, *OperatorState) error
	UpdateOperatorID(OperatorID)
}

// chunkValidator implements the validation logic that a DA node should apply to its recieved chunks
type chunkValidator struct {
	encoder    Encoder
	assignment AssignmentCoordinator
	chainState ChainState
	operatorID OperatorID
}

func NewChunkValidator(enc Encoder, asgn AssignmentCoordinator, cst ChainState, operatorID OperatorID) ChunkValidator {
	return &chunkValidator{
		encoder:    enc,
		assignment: asgn,
		chainState: cst,
		operatorID: operatorID,
	}
}

func (v *chunkValidator) ValidateBlob(blob *BlobMessage, operatorState *OperatorState) error {
	if len(blob.Bundles) != len(blob.BlobHeader.QuorumInfos) {
		return errors.New("number of bundles does not match number of quorums")
	}

	// Validate the blob length
	err := v.encoder.VerifyBlobLength(blob.BlobHeader.BlobCommitments)
	if err != nil {
		return err
	}

	for _, quorumHeader := range blob.BlobHeader.QuorumInfos {

		if quorumHeader.AdversaryThreshold >= quorumHeader.QuorumThreshold {
			return errors.New("invalid header: quorum threshold does not exceed adversary threshold")
		}

		// Check if the operator is a member of the quorum
		if _, ok := operatorState.Operators[quorumHeader.QuorumID]; !ok {
			continue
		}

		// Get the assignments for the quorum
		assignment, info, err := v.assignment.GetOperatorAssignment(operatorState, quorumHeader.QuorumID, quorumHeader.QuantizationFactor, v.operatorID)
		if err != nil {
			return err
		}

		// Validate the number of chunks
		if assignment.NumChunks == 0 {
			continue
		}
		if assignment.NumChunks != uint(len(blob.Bundles[quorumHeader.QuorumID])) {
			return errors.New("number of chunks does not match assignment")
		}

		chunkLength, err := v.assignment.GetChunkLengthFromHeader(operatorState, quorumHeader)
		if err != nil {
			return err
		}

		// Validate the chunkLength against the quorum and adversary threshold parameters
		numOperators := uint(len(operatorState.Operators[quorumHeader.QuorumID]))
		minChunkLength, err := v.assignment.GetMinimumChunkLength(numOperators, blob.BlobHeader.BlobCommitments.Length, quorumHeader.QuantizationFactor, quorumHeader.QuorumThreshold, quorumHeader.AdversaryThreshold)
		if err != nil {
			return err
		}
		params, err := GetEncodingParams(minChunkLength, info.TotalChunks)
		if err != nil {
			return err
		}

		if params.ChunkLength != chunkLength {
			return errors.New("number of chunks does not match assignment")
		}

		// Get the chunk length
		chunks := blob.Bundles[quorumHeader.QuorumID]
		for _, chunk := range chunks {
			if uint(chunk.Length()) != chunkLength {
				return ErrChunkLengthMismatch
			}
		}

		// Validate the chunk length
		if chunkLength*quorumHeader.QuantizationFactor*numOperators != quorumHeader.EncodedBlobLength {
			return ErrInvalidHeader
		}

		// Check the received chunks against the commitment
		err = v.encoder.VerifyChunks(chunks, assignment.GetIndices(), blob.BlobHeader.BlobCommitments, params)
		if err != nil {
			return err
		}

	}

	return nil
}

func (v *chunkValidator) UpdateOperatorID(operatorID OperatorID) {
	v.operatorID = operatorID
}
