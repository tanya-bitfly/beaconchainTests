package modules

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/gobitfly/beaconchain/pkg/commons/db"
	"github.com/gobitfly/beaconchain/pkg/commons/utils"
	edb "github.com/gobitfly/beaconchain/pkg/exporter/db"
	"github.com/pkg/errors"
)

type epochToHourAggregator struct {
	*dashboardData
	mutex *sync.Mutex
}

const hourRetentionBuffer = 1.2 // do not go below 1

func getHourAggregateWidth() uint64 {
	return utils.EpochsPerDay() / 24
}

func newEpochToHourAggregator(d *dashboardData) *epochToHourAggregator {
	return &epochToHourAggregator{
		dashboardData: d,
		mutex:         &sync.Mutex{},
	}
}

func (d *epochToHourAggregator) clearOldHourAggregations(removeBelowEpoch int64) error {
	partitions, err := edb.GetPartitionNamesOfTable("validator_dashboard_data_hourly")
	if err != nil {
		return errors.Wrap(err, "failed to get partitions")
	}

	for _, partition := range partitions {
		epochFrom, epochTo, err := parseEpochRange(`validator_dashboard_data_hourly_(\d+)_(\d+)`, partition)
		if err != nil {
			return errors.Wrap(err, "failed to parse epoch range")
		}

		if int64(epochTo) < removeBelowEpoch {
			d.mutex.Lock()
			err := d.deleteHourlyPartition(epochFrom, epochTo)
			d.log.Infof("Deleted old hourly partition %d-%d", epochFrom, epochTo)
			d.mutex.Unlock()
			if err != nil {
				return errors.Wrap(err, "failed to delete hourly partition")
			}
		}
	}

	return nil
}

// Assumes no gaps in epochs
func (d *epochToHourAggregator) aggregate1h(currentExportedEpoch uint64) error {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	startTime := time.Now()
	d.log.Info("aggregating 1h")
	defer func() {
		d.log.Infof("aggregate 1h took %v", time.Since(startTime))
	}()

	lastHourExported, err := edb.GetLastExportedHour()
	if err != nil && err != sql.ErrNoRows {
		return errors.Wrap(err, "failed to get latest dashboard hourly epoch")
	}

	differenceToCurrentEpoch := currentExportedEpoch + 1 - lastHourExported.EpochEnd // epochEnd is excl hence the +1

	if differenceToCurrentEpoch > d.getHourRetentionDurationEpochs() {
		d.log.Warnf("difference to current epoch is larger than retention duration, skipping for now: %v", differenceToCurrentEpoch)
		return nil
	}

	gaps, err := edb.GetDashboardEpochGapsBetween(currentExportedEpoch, int64(currentExportedEpoch+1-d.epochWriter.getRetentionEpochDuration()))
	if err != nil {
		return errors.Wrap(err, "failed to get dashboard epoch gaps")
	}

	if len(gaps) > 0 {
		return fmt.Errorf("gaps in dashboard epoch, skipping for now: %v", gaps) // sanity, this should never happen
	}

	_, currentEndBound := getHourAggregateBounds(currentExportedEpoch)

	for epoch := lastHourExported.EpochStart; epoch <= currentEndBound; epoch += getHourAggregateWidth() {
		boundsStart, boundsEnd := getHourAggregateBounds(epoch)
		d.log.Infof("epoch: %d, boundsStart: %d, boundsEnd: %d |  lastHourExported: %v", epoch, boundsStart, boundsEnd, lastHourExported)
		if lastHourExported.EpochEnd == boundsEnd { // no need to update last hour entry if it is complete
			d.log.Infof("skipping updating last hour entry since it is complete")
			continue
		}

		// define start bounds as lastHourExported.EpochEnd for first iteration
		if epoch == lastHourExported.EpochStart {
			boundsStart = lastHourExported.EpochEnd
		}

		// scope down to max currentExportedEpoch (since epoch data is inclusive, add 1)
		if currentExportedEpoch+1 >= boundsStart && currentExportedEpoch+1 < boundsEnd {
			boundsEnd = currentExportedEpoch + 1
		}

		err = d.aggregate1hSpecific(boundsStart, boundsEnd)
		if err != nil {
			return errors.Wrap(err, "failed to aggregate 1h")
		}
	}

	d.log.Info("finished 1h aggregation")

	return nil
}

func getHourAggregateBounds(epoch uint64) (uint64, uint64) {
	offset := utils.GetEpochOffsetGenesis()
	epoch += offset                                                               // offset to utc
	startOfPartition := epoch / getHourAggregateWidth() * getHourAggregateWidth() // inclusive
	endOfPartition := startOfPartition + getHourAggregateWidth()                  // exclusive
	if startOfPartition < offset {
		startOfPartition = offset
	}
	return startOfPartition - offset, endOfPartition - offset
}

func (d *epochToHourAggregator) GetHourPartitionRange(epoch uint64) (uint64, uint64) {
	startOfPartition := epoch / (PartitionEpochWidth * getHourAggregateWidth()) * PartitionEpochWidth * getHourAggregateWidth() // inclusive
	endOfPartition := startOfPartition + PartitionEpochWidth*getHourAggregateWidth()                                            // exclusive
	return startOfPartition, endOfPartition
}

func (d *epochToHourAggregator) getHourRetentionDurationEpochs() uint64 {
	return uint64(float64(utils.EpochsPerDay()) * hourRetentionBuffer)
}

func (d *epochToHourAggregator) createHourlyPartition(epochStartFrom, epochStartTo uint64) error {
	_, err := db.AlloyWriter.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS validator_dashboard_data_hourly_%d_%d 
		PARTITION OF validator_dashboard_data_hourly
			FOR VALUES FROM (%[1]d) TO (%[2]d)
		`,
		epochStartFrom, epochStartTo,
	))
	return err
}

func (d *epochToHourAggregator) deleteHourlyPartition(epochStartFrom, epochStartTo uint64) error {
	_, err := db.AlloyWriter.Exec(fmt.Sprintf(`
		DROP TABLE IF EXISTS validator_dashboard_data_hourly_%d_%d
		`,
		epochStartFrom, epochStartTo,
	))

	return err
}

// epochStart incl, epochEnd excl
func (d *epochToHourAggregator) aggregate1hSpecific(epochStart, epochEnd uint64) error {
	tx, err := db.AlloyWriter.Beginx()
	if err != nil {
		return errors.Wrap(err, "failed to start transaction")
	}
	defer utils.Rollback(tx)

	partitionStartRange, partitionEndRange := d.GetHourPartitionRange(epochStart)

	err = d.createHourlyPartition(partitionStartRange, partitionEndRange)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to create hourly partition, startRange: %d, endRange: %d", partitionStartRange, partitionEndRange))
	}

	boundsStart, _ := getHourAggregateBounds(epochStart)

	if epochStart == partitionStartRange && debugAddToColumnEngine {
		err = edb.AddToColumnEngineAllColumns(fmt.Sprintf("validator_dashboard_data_hourly_%d_%d", partitionStartRange, partitionEndRange))
		if err != nil {
			d.log.Warnf("Failed to add epoch to column engine: %v", err)
		}
	}

	d.log.Infof("aggregating 1h specific, startEpoch: %d endEpoch: %d", epochStart, epochEnd)

	err = AddToRollingCustom(tx, CustomRolling{
		StartEpoch:           epochStart,
		EndEpoch:             epochEnd - 1, // rolling arg is inclusive
		StartBoundEpoch:      int64(boundsStart),
		TableFrom:            "validator_dashboard_data_epoch",
		TableTo:              "validator_dashboard_data_hourly",
		TableFromEpochColumn: "epoch",
		Log:                  d.log,
		TailBalancesQuery: `balance_starts as (
				SELECT validator_index, balance_start FROM validator_dashboard_data_epoch WHERE epoch = $3
		),`,
		TailBalancesJoinQuery:         `LEFT JOIN balance_starts ON aggregate_head.validator_index = balance_starts.validator_index`,
		TailBalancesInsertColumnQuery: `balance_start,`,
		TableConflict:                 "(epoch_start, validator_index)",
	})

	if err != nil {
		return errors.Wrap(err, "failed to insert hourly data")
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "failed to commit transaction")
	}
	return nil

	_, err = tx.Exec(`
		WITH
			end_epoch as (
				SELECT max(epoch) as epoch FROM validator_dashboard_data_epoch where epoch < $2 AND epoch >= $3
			),
			balance_starts as (
				SELECT validator_index, balance_start FROM validator_dashboard_data_epoch WHERE epoch = $3
			),
			balance_ends as (
				SELECT validator_index, balance_end FROM validator_dashboard_data_epoch WHERE epoch = (SELECT epoch FROM end_epoch)
			),
			aggregate as (
				SELECT 
					validator_index,
					SUM(attestations_source_reward) as attestations_source_reward,
					SUM(attestations_target_reward) as attestations_target_reward,
					SUM(attestations_head_reward) as attestations_head_reward,
					SUM(attestations_inactivity_reward) as attestations_inactivity_reward,
					SUM(attestations_inclusion_reward) as attestations_inclusion_reward,
					SUM(attestations_reward) as attestations_reward,
					SUM(attestations_ideal_source_reward) as attestations_ideal_source_reward,
					SUM(attestations_ideal_target_reward) as attestations_ideal_target_reward,
					SUM(attestations_ideal_head_reward) as attestations_ideal_head_reward,
					SUM(attestations_ideal_inactivity_reward) as attestations_ideal_inactivity_reward,
					SUM(attestations_ideal_inclusion_reward) as attestations_ideal_inclusion_reward,
					SUM(attestations_ideal_reward) as attestations_ideal_reward,
					SUM(blocks_scheduled) as blocks_scheduled,
					SUM(blocks_proposed) as blocks_proposed,
					SUM(blocks_cl_reward) as blocks_cl_reward,
					SUM(sync_scheduled) as sync_scheduled,
					SUM(sync_executed) as sync_executed,
					SUM(sync_rewards) as sync_rewards,
					bool_or(slashed) as slashed,
					SUM(deposits_count) as deposits_count,
					SUM(deposits_amount) as deposits_amount,
					SUM(withdrawals_count) as withdrawals_count,
					SUM(withdrawals_amount) as withdrawals_amount,
					SUM(inclusion_delay_sum) as inclusion_delay_sum,
					SUM(block_chance) as block_chance,
					SUM(attestations_scheduled) as attestations_scheduled,
					SUM(attestations_executed) as attestations_executed,
					SUM(attestation_head_executed) as attestation_head_executed,
					SUM(attestation_source_executed) as attestation_source_executed,
					SUM(attestation_target_executed) as attestation_target_executed,
					SUM(optimal_inclusion_delay_sum) as optimal_inclusion_delay_sum,
					SUM(slasher_reward) as slasher_reward,
					MAX(slashed_by) as slashed_by,
					MAX(slashed_violation) as slashed_violation,
					MAX(last_executed_duty_epoch) as last_executed_duty_epoch					
				FROM validator_dashboard_data_epoch
				WHERE epoch >= $1 AND epoch < $2
				GROUP BY validator_index
			)
			INSERT INTO validator_dashboard_data_hourly (
				epoch_start,
				epoch_end,
				validator_index,
				attestations_source_reward,
				attestations_target_reward,
				attestations_head_reward,
				attestations_inactivity_reward,
				attestations_inclusion_reward,
				attestations_reward,
				attestations_ideal_source_reward,
				attestations_ideal_target_reward,
				attestations_ideal_head_reward,
				attestations_ideal_inactivity_reward,
				attestations_ideal_inclusion_reward,
				attestations_ideal_reward,
				blocks_scheduled,
				blocks_proposed,
				blocks_cl_reward,
				sync_scheduled,
				sync_executed,
				sync_rewards,
				slashed,
				balance_start,
				balance_end,
				deposits_count,
				deposits_amount,
				withdrawals_count,
				withdrawals_amount,
				inclusion_delay_sum,
				block_chance,
				attestations_scheduled,
				attestations_executed,
				attestation_head_executed,
				attestation_source_executed,
				attestation_target_executed,
				optimal_inclusion_delay_sum,
				slashed_by,
				slashed_violation,
				slasher_reward,
				last_executed_duty_epoch
			)
			SELECT 
				$3,
				(SELECT epoch FROM end_epoch) + 1 as epoch, -- exclusive
				aggregate.validator_index,
				attestations_source_reward,
				attestations_target_reward,
				attestations_head_reward,
				attestations_inactivity_reward,
				attestations_inclusion_reward,
				attestations_reward,
				attestations_ideal_source_reward,
				attestations_ideal_target_reward,
				attestations_ideal_head_reward,
				attestations_ideal_inactivity_reward,
				attestations_ideal_inclusion_reward,
				attestations_ideal_reward,
				blocks_scheduled,
				blocks_proposed,
				blocks_cl_reward,
				sync_scheduled,
				sync_executed,
				sync_rewards,
				slashed,
				balance_start,
				balance_end,
				deposits_count,
				deposits_amount,
				withdrawals_count,
				withdrawals_amount,
				inclusion_delay_sum,
				block_chance,
				attestations_scheduled,
				attestations_executed,
				attestation_head_executed,
				attestation_source_executed,
				attestation_target_executed,
				optimal_inclusion_delay_sum,
				slashed_by,
				slashed_violation,
				slasher_reward,
				last_executed_duty_epoch
			FROM aggregate
			LEFT JOIN balance_starts ON aggregate.validator_index = balance_starts.validator_index
			LEFT JOIN balance_ends ON aggregate.validator_index = balance_ends.validator_index
			ON CONFLICT (epoch_start, validator_index) DO UPDATE SET
				attestations_source_reward = validator_dashboard_data_hourly.attestations_source_reward + EXCLUDED.attestations_source_reward,
				attestations_target_reward = validator_dashboard_data_hourly.attestations_target_reward + EXCLUDED.attestations_target_reward,
				attestations_head_reward = validator_dashboard_data_hourly.attestations_head_reward + EXCLUDED.attestations_head_reward,
				attestations_inactivity_reward = validator_dashboard_data_hourly.attestations_inactivity_reward + EXCLUDED.attestations_inactivity_reward,
				attestations_inclusion_reward = validator_dashboard_data_hourly.attestations_inclusion_reward + EXCLUDED.attestations_inclusion_reward,
				attestations_reward = validator_dashboard_data_hourly.attestations_reward + EXCLUDED.attestations_reward,
				attestations_ideal_source_reward = validator_dashboard_data_hourly.attestations_ideal_source_reward + EXCLUDED.attestations_ideal_source_reward,
				attestations_ideal_target_reward = validator_dashboard_data_hourly.attestations_ideal_target_reward + EXCLUDED.attestations_ideal_target_reward,
				attestations_ideal_head_reward = validator_dashboard_data_hourly.attestations_ideal_head_reward + EXCLUDED.attestations_ideal_head_reward,
				attestations_ideal_inactivity_reward = validator_dashboard_data_hourly.attestations_ideal_inactivity_reward + EXCLUDED.attestations_ideal_inactivity_reward,
				attestations_ideal_inclusion_reward = validator_dashboard_data_hourly.attestations_ideal_inclusion_reward + EXCLUDED.attestations_ideal_inclusion_reward,
				attestations_ideal_reward = validator_dashboard_data_hourly.attestations_ideal_reward + EXCLUDED.attestations_ideal_reward,
				blocks_scheduled = validator_dashboard_data_hourly.blocks_scheduled + EXCLUDED.blocks_scheduled,
				blocks_proposed = validator_dashboard_data_hourly.blocks_proposed + EXCLUDED.blocks_proposed,
				blocks_cl_reward = validator_dashboard_data_hourly.blocks_cl_reward + EXCLUDED.blocks_cl_reward,
				sync_scheduled = validator_dashboard_data_hourly.sync_scheduled + EXCLUDED.sync_scheduled,
				sync_executed = validator_dashboard_data_hourly.sync_executed + EXCLUDED.sync_executed,
				sync_rewards = validator_dashboard_data_hourly.sync_rewards + EXCLUDED.sync_rewards,
				slashed = COALESCE(validator_dashboard_data_hourly.slashed, EXCLUDED.slashed),
				balance_end = EXCLUDED.balance_end,
				deposits_count = validator_dashboard_data_hourly.deposits_count + EXCLUDED.deposits_count,
				deposits_amount = validator_dashboard_data_hourly.deposits_amount + EXCLUDED.deposits_amount,
				withdrawals_count = validator_dashboard_data_hourly.withdrawals_count + EXCLUDED.withdrawals_count,
				withdrawals_amount = validator_dashboard_data_hourly.withdrawals_amount + EXCLUDED.withdrawals_amount,
				inclusion_delay_sum = validator_dashboard_data_hourly.inclusion_delay_sum + EXCLUDED.inclusion_delay_sum,
				block_chance = validator_dashboard_data_hourly.block_chance + EXCLUDED.block_chance,
				attestations_scheduled = validator_dashboard_data_hourly.attestations_scheduled + EXCLUDED.attestations_scheduled,
				attestations_executed = validator_dashboard_data_hourly.attestations_executed + EXCLUDED.attestations_executed,
				attestation_head_executed = validator_dashboard_data_hourly.attestation_head_executed + EXCLUDED.attestation_head_executed,
				attestation_source_executed = validator_dashboard_data_hourly.attestation_source_executed + EXCLUDED.attestation_source_executed,
				attestation_target_executed = validator_dashboard_data_hourly.attestation_target_executed + EXCLUDED.attestation_target_executed,
				optimal_inclusion_delay_sum = validator_dashboard_data_hourly.optimal_inclusion_delay_sum + EXCLUDED.optimal_inclusion_delay_sum,
				slasher_reward = validator_dashboard_data_hourly.slasher_reward + EXCLUDED.slasher_reward,
				slashed_by = COALESCE(validator_dashboard_data_hourly.slashed_by, EXCLUDED.slashed_by),
				slashed_violation = COALESCE(validator_dashboard_data_hourly.slashed_violation, EXCLUDED.slashed_violation),
				last_executed_duty_epoch = COALESCE(validator_dashboard_data_hourly.last_executed_duty_epoch, EXCLUDED.last_executed_duty_epoch),
				epoch_end = EXCLUDED.epoch_end
	`, epochStart, epochEnd, boundsStart)

	if err != nil {
		return errors.Wrap(err, "failed to insert hourly data")
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "failed to commit transaction")
	}

	return nil
}
