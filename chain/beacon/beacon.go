package beacon

import (
	"context"

	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/types"
)

var log = logging.Logger("beacon")

type Response struct {
	Entry types.BeaconEntry
	Err   error
}

type Schedule []BeaconPoint

func (bs Schedule) BeaconForEpoch(e abi.ChainEpoch) RandomBeacon {
	for i := len(bs) - 1; i >= 0; i-- {
		bp := bs[i]
		if e >= bp.Start {
			return bp.Beacon
		}
	}
	return bs[0].Beacon
}

type BeaconPoint struct {
	Start  abi.ChainEpoch
	Beacon RandomBeacon
}

// RandomBeacon represents a system that provides randomness to Lotus.
// Other components interrogate the RandomBeacon to acquire randomness that's
// valid for a specific chain epoch. Also to verify beacon entries that have
// been posted on chain.
type RandomBeacon interface {
	Entry(context.Context, uint64) <-chan Response
	VerifyEntry(types.BeaconEntry, types.BeaconEntry) error
	MaxBeaconRoundForEpoch(network.Version, abi.ChainEpoch) uint64
}

func ValidateBlockValues(bSchedule Schedule, nv network.Version, h *types.BlockHeader, parentEpoch abi.ChainEpoch,
	prevEntry types.BeaconEntry) error {
	{
		parentBeacon := bSchedule.BeaconForEpoch(parentEpoch)
		currBeacon := bSchedule.BeaconForEpoch(h.Height)
		if parentBeacon != currBeacon {
			if len(h.BeaconEntries) != 2 {
				return xerrors.Errorf("expected two beacon entries at beacon fork, got %d", len(h.BeaconEntries))
			}
			err := currBeacon.VerifyEntry(h.BeaconEntries[1], h.BeaconEntries[0])
			if err != nil {
				return xerrors.Errorf("beacon at fork point invalid: (%v, %v): %w",
					h.BeaconEntries[1], h.BeaconEntries[0], err)
			}
			return nil
		}
	}

	// TODO: fork logic
	b := bSchedule.BeaconForEpoch(h.Height)
	maxRound := b.MaxBeaconRoundForEpoch(nv, h.Height)
	if maxRound == prevEntry.Round {
		if len(h.BeaconEntries) != 0 {
			return xerrors.Errorf("expected not to have any beacon entries in this block, got %d", len(h.BeaconEntries))
		}
		return nil
	}

	if len(h.BeaconEntries) == 0 {
		return xerrors.Errorf("expected to have beacon entries in this block, but didn't find any")
	}

	// Verify that the last beacon entry's round corresponds to the round we expect
	last := h.BeaconEntries[len(h.BeaconEntries)-1]
	if last.Round != maxRound {
		return xerrors.Errorf("expected final beacon entry in block to be at round %d, got %d", maxRound, last.Round)
	}

	// Verify that all other entries' rounds are as expected for the epochs in between parentEpoch and h.Height
	for i, e := range h.BeaconEntries {
		correctRound := b.MaxBeaconRoundForEpoch(nv, parentEpoch+abi.ChainEpoch(i)+1)
		if e.Round != correctRound {
			return xerrors.Errorf("unexpected beacon round %d, expected %d for epoch %d", e.Round, correctRound, parentEpoch+abi.ChainEpoch(i))
		}
	}

	// Verify the beacon entries themselves
	for i, e := range h.BeaconEntries {
		if err := b.VerifyEntry(e, prevEntry); err != nil {
			return xerrors.Errorf("beacon entry %d (%d - %x (%d)) was invalid: %w", i, e.Round, e.Data, len(e.Data), err)
		}
		prevEntry = e
	}

	return nil
}

func BeaconEntriesForBlock(ctx context.Context, bSchedule Schedule, nv network.Version, epoch abi.ChainEpoch, parentEpoch abi.ChainEpoch, prev types.BeaconEntry) ([]types.BeaconEntry, error) {
	{
		parentBeacon := bSchedule.BeaconForEpoch(parentEpoch)
		currBeacon := bSchedule.BeaconForEpoch(epoch)
		if parentBeacon != currBeacon {
			// Fork logic
			round := currBeacon.MaxBeaconRoundForEpoch(nv, epoch)
			out := make([]types.BeaconEntry, 2)
			rch := currBeacon.Entry(ctx, round-1)
			res := <-rch
			if res.Err != nil {
				return nil, xerrors.Errorf("getting entry %d returned error: %w", round-1, res.Err)
			}
			out[0] = res.Entry
			rch = currBeacon.Entry(ctx, round)
			res = <-rch
			if res.Err != nil {
				return nil, xerrors.Errorf("getting entry %d returned error: %w", round, res.Err)
			}
			out[1] = res.Entry
			return out, nil
		}
	}

	beacon := bSchedule.BeaconForEpoch(epoch)

	start := build.Clock.Now()

	maxRound := beacon.MaxBeaconRoundForEpoch(nv, epoch)
	if maxRound == prev.Round {
		return nil, nil
	}

	// TODO: this is a sketchy way to handle the genesis block not having a beacon entry
	if prev.Round == 0 {
		prev.Round = maxRound - 1
	}

	var out []types.BeaconEntry
	for currEpoch := epoch; currEpoch > parentEpoch; currEpoch-- {
		currRound := beacon.MaxBeaconRoundForEpoch(nv, currEpoch)
		rch := beacon.Entry(ctx, currRound)
		select {
		case resp := <-rch:
			if resp.Err != nil {
				return nil, xerrors.Errorf("beacon entry request returned error: %w", resp.Err)
			}

			out = append(out, resp.Entry)
		case <-ctx.Done():
			return nil, xerrors.Errorf("context timed out waiting on beacon entry to come back for epoch %d: %w", epoch, ctx.Err())
		}
	}

	log.Debugw("fetching beacon entries", "took", build.Clock.Since(start), "numEntries", len(out))
	reverse(out)
	return out, nil
}

func reverse(arr []types.BeaconEntry) {
	for i := 0; i < len(arr)/2; i++ {
		arr[i], arr[len(arr)-(1+i)] = arr[len(arr)-(1+i)], arr[i]
	}
}
