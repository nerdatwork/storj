// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package metabase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeebo/errs"

	"storj.io/common/storj"
	"storj.io/common/uuid"
	"storj.io/storj/private/dbutil/pgutil"
	"storj.io/storj/private/dbutil/txutil"
	"storj.io/storj/private/tagsql"
)

// CommitObjectWithSegments contains arguments necessary for committing an object.
type CommitObjectWithSegments struct {
	ObjectStream

	EncryptedMetadata             []byte
	EncryptedMetadataNonce        []byte
	EncryptedMetadataEncryptedKey []byte

	// TODO: this probably should use segment ranges rather than individual items
	Segments []SegmentPosition
}

// CommitObjectWithSegments commits pending object to the database.
func (db *DB) CommitObjectWithSegments(ctx context.Context, opts CommitObjectWithSegments) (object Object, deletedSegments []DeletedSegmentInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.ObjectStream.Verify(); err != nil {
		return Object{}, nil, err
	}
	if err := verifySegmentOrder(opts.Segments); err != nil {
		return Object{}, nil, err
	}

	err = txutil.WithTx(ctx, db.db, nil, func(ctx context.Context, tx tagsql.Tx) error {
		// TODO: should we prevent this from executing when the object has been committed
		// currently this requires quite a lot of database communication, so invalid handling can be expensive.

		segmentsInDatabase, err := fetchSegmentsForCommit(ctx, tx, opts.StreamID)
		if err != nil {
			return err
		}

		finalSegments, segmentsToDelete, err := determineCommitActions(opts.Segments, segmentsInDatabase)
		if err != nil {
			return err
		}

		err = updateSegmentOffsets(ctx, tx, opts.StreamID, finalSegments)
		if err != nil {
			return err
		}

		deletedSegments, err = deleteSegmentsNotInCommit(ctx, tx, opts.StreamID, segmentsToDelete)
		if err != nil {
			return err
		}

		// TODO: would we even need this when we make main index plain_offset?
		fixedSegmentSize := int32(0)
		if len(finalSegments) > 0 {
			fixedSegmentSize = finalSegments[0].EncryptedSize
			for i, seg := range finalSegments {
				if seg.Position.Part != 0 {
					fixedSegmentSize = -1
					break
				}
				if i < len(finalSegments)-1 && seg.EncryptedSize != fixedSegmentSize {
					fixedSegmentSize = -1
					break
				}
			}
		}

		var totalPlainSize, totalEncryptedSize int64
		for _, seg := range finalSegments {
			totalPlainSize += int64(seg.PlainSize)
			totalEncryptedSize += int64(seg.EncryptedSize)
		}

		err = tx.QueryRow(ctx, `
			UPDATE objects SET
				status =`+committedStatus+`,
				segment_count = $6,

				encrypted_metadata_nonce         = $7,
				encrypted_metadata               = $8,
				encrypted_metadata_encrypted_key = $9,

				total_plain_size     = $10,
				total_encrypted_size = $11,
				fixed_segment_size   = $12,
				zombie_deletion_deadline = NULL
			WHERE
				project_id   = $1 AND
				bucket_name  = $2 AND
				object_key   = $3 AND
				version      = $4 AND
				stream_id    = $5 AND
				status       = `+pendingStatus+`
			RETURNING
				created_at, expires_at,
				encryption;
		`, opts.ProjectID, opts.BucketName, []byte(opts.ObjectKey), opts.Version, opts.StreamID,
			len(finalSegments),
			opts.EncryptedMetadataNonce, opts.EncryptedMetadata, opts.EncryptedMetadataEncryptedKey,
			totalPlainSize,
			totalEncryptedSize,
			fixedSegmentSize,
		).
			Scan(
				&object.CreatedAt, &object.ExpiresAt,
				encryptionParameters{&object.Encryption},
			)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storj.ErrObjectNotFound.Wrap(Error.New("object with specified version and pending status is missing"))
			}
			return Error.New("failed to update object: %w", err)
		}

		object.StreamID = opts.StreamID
		object.ProjectID = opts.ProjectID
		object.BucketName = opts.BucketName
		object.ObjectKey = opts.ObjectKey
		object.Version = opts.Version
		object.Status = Committed
		object.SegmentCount = int32(len(finalSegments))
		object.EncryptedMetadataNonce = opts.EncryptedMetadataNonce
		object.EncryptedMetadata = opts.EncryptedMetadata
		object.EncryptedMetadataEncryptedKey = opts.EncryptedMetadataEncryptedKey
		object.TotalPlainSize = totalPlainSize
		object.TotalEncryptedSize = totalEncryptedSize
		object.FixedSegmentSize = fixedSegmentSize
		return nil
	})
	if err != nil {
		return Object{}, nil, err
	}
	return object, deletedSegments, nil
}

func verifySegmentOrder(positions []SegmentPosition) error {
	if len(positions) == 0 {
		return nil
	}

	last := positions[0]
	for _, next := range positions[1:] {
		if !last.Less(next) {
			return Error.New("segments not in ascending order, got %v before %v", last, next)
		}
		last = next
	}

	return nil
}

// segmentInfoForCommit is database state prior to deleting objects.
type segmentInfoForCommit struct {
	Position      SegmentPosition
	EncryptedSize int32
	PlainOffset   int64
	PlainSize     int32
}

// fetchSegmentsForCommit loads information necessary for validating segment existence and offsets.
func fetchSegmentsForCommit(ctx context.Context, tx tagsql.Tx, streamID uuid.UUID) (segments []segmentInfoForCommit, err error) {
	defer mon.Task()(&ctx)(&err)

	err = withRows(tx.Query(ctx, `
		SELECT position, encrypted_size, plain_offset, plain_size
		FROM segments
		WHERE stream_id = $1
		ORDER BY position
	`, streamID))(func(rows tagsql.Rows) error {
		for rows.Next() {
			var segment segmentInfoForCommit
			err := rows.Scan(&segment.Position, &segment.EncryptedSize, &segment.PlainOffset, &segment.PlainSize)
			if err != nil {
				return Error.New("failed to scan segments: %w", err)
			}
			segments = append(segments, segment)
		}
		return nil
	})
	if err != nil {
		return nil, Error.New("failed to fetch segments: %w", err)
	}
	return segments, nil
}

type segmentToCommit struct {
	Position       SegmentPosition
	OldPlainOffset int64
	PlainSize      int32
	EncryptedSize  int32
}

// determineCommitActions detects how should the database be updated and which segments should be deleted.
func determineCommitActions(segments []SegmentPosition, segmentsInDatabase []segmentInfoForCommit) (commit []segmentToCommit, toDelete []SegmentPosition, err error) {
	var invalidSegments errs.Group

	commit = make([]segmentToCommit, 0, len(segments))
	diffSegmentsWithDatabase(segments, segmentsInDatabase, func(a *SegmentPosition, b *segmentInfoForCommit) {
		// If we do not have an appropriate segment in the database it means
		// either the segment was deleted before commit finished or the
		// segment was not uploaded. Either way we need to fail the commit.
		if b == nil {
			invalidSegments.Add(fmt.Errorf("%v: segment not committed", *a))
			return
		}

		// If we do not commit a segment that's in a database we should delete them.
		// This could happen when the user tries to upload a segment,
		// fails, reuploads and then during commit decides to not commit into the object.
		if a == nil {
			toDelete = append(toDelete, b.Position)
			return
		}

		commit = append(commit, segmentToCommit{
			Position:       *a,
			OldPlainOffset: b.PlainOffset,
			PlainSize:      b.PlainSize,
			EncryptedSize:  b.EncryptedSize,
		})
	})

	if err := invalidSegments.Err(); err != nil {
		return nil, nil, Error.New("segments and database does not match: %v", err)
	}
	return commit, toDelete, nil
}

// updateSegmentOffsets updates segment offsets that didn't match the database state.
func updateSegmentOffsets(ctx context.Context, tx tagsql.Tx, streamID uuid.UUID, updates []segmentToCommit) (err error) {
	defer mon.Task()(&ctx)(&err)
	if len(updates) == 0 {
		return
	}

	// We may be able to skip this, if the database state have been already submitted
	// and the plain offsets haven't changed.

	// Update plain offsets of the segments.
	var update struct {
		Positions    []int64
		PlainOffsets []int64
	}
	expectedOffset := int64(0)
	for _, u := range updates {
		if u.OldPlainOffset != expectedOffset {
			update.Positions = append(update.Positions, int64(u.Position.Encode()))
			update.PlainOffsets = append(update.PlainOffsets, expectedOffset)
		}
		expectedOffset += int64(u.PlainSize)
	}

	if len(update.Positions) == 0 {
		return nil
	}

	updateResult, err := tx.Exec(ctx, `
			UPDATE segments
			SET plain_offset = P.plain_offset
			FROM (SELECT unnest($2::INT8[]), unnest($3::INT8[])) as P(position, plain_offset)
			WHERE segments.stream_id = $1 AND segments.position = P.position
		`, streamID, pgutil.Int8Array(update.Positions), pgutil.Int8Array(update.PlainOffsets))
	if err != nil {
		return Error.New("unable to update segments offsets: %w", err)
	}
	affected, err := updateResult.RowsAffected()
	if err != nil {
		return Error.New("unable to get number of affected segments: %w", err)
	}
	if affected != int64(len(update.Positions)) {
		return Error.New("not all segments were updated, expected %d got %d", len(update.Positions), affected)
	}

	return nil
}

// deleteSegmentsNotInCommit deletes the listed segments inside the tx.
func deleteSegmentsNotInCommit(ctx context.Context, tx tagsql.Tx, streamID uuid.UUID, segments []SegmentPosition) (deletedSegments []DeletedSegmentInfo, err error) {
	defer mon.Task()(&ctx)(&err)
	if len(segments) == 0 {
		return nil, nil
	}

	positions := []int64{}
	for _, p := range segments {
		positions = append(positions, int64(p.Encode()))
	}

	// This potentially could be done together with the previous database call.
	err = withRows(tx.Query(ctx, `
			DELETE FROM segments
			WHERE stream_id = $1 AND position = ANY($2)
			RETURNING root_piece_id, remote_pieces
		`, streamID, pgutil.Int8Array(positions)))(func(rows tagsql.Rows) error {
		for rows.Next() {
			var deleted DeletedSegmentInfo
			err := rows.Scan(&deleted.RootPieceID, &deleted.Pieces)
			if err != nil {
				return Error.New("failed to scan segments: %w", err)
			}
			// we don't need to report info about inline segments
			if deleted.RootPieceID.IsZero() {
				continue
			}
			deletedSegments = append(deletedSegments, deleted)
		}
		return nil
	})
	if err != nil {
		return nil, Error.New("unable to delete segments: %w", err)
	}

	return deletedSegments, nil
}

// diffSegmentsWithDatabase matches up segment positions with their database information.
func diffSegmentsWithDatabase(as []SegmentPosition, bs []segmentInfoForCommit, cb func(a *SegmentPosition, b *segmentInfoForCommit)) {
	for len(as) > 0 && len(bs) > 0 {
		if as[0] == bs[0].Position {
			cb(&as[0], &bs[0])
			as, bs = as[1:], bs[1:]
		} else if as[0].Less(bs[0].Position) {
			cb(&as[0], nil)
			as = as[1:]
		} else {
			cb(nil, &bs[0])
			bs = bs[1:]
		}
	}
	for i := range as {
		cb(&as[i], nil)
	}
	for i := range bs {
		cb(nil, &bs[i])
	}
}