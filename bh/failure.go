package bh

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang/glog"
)

type failureHandler struct {
	lockTimeout time.Duration
}

func (h *failureHandler) Rcv(msg Msg, ctx RcvContext) error {
	ctx.AbortTx()

	bFailed := msg.Data().(beeFailed)
	b, ok := ctx.(*localBee)
	if !ok {
		return errors.New("Cannot handle failures of a detached or a proxy.")
	}

	if err := b.hive.registry.tryLockApp(b.id()); err != nil {
		ctx.Snooze(h.lockTimeout)
	}

	defer func() {
		if err := b.hive.registry.unlockApp(b.id()); err != nil {
			glog.Fatalf("Cannot unlock the application: %v", err)
		}
	}()

	bCol := b.colony()
	switch {
	case bCol.IsMaster(bFailed.id):
		b.handleMasterFailure(bFailed.id)

	case bCol.IsSlave(bFailed.id) && b.isMaster():
		b.handleSlaveFailure(bFailed.id)
	}
	return nil
}

func (h *failureHandler) Map(msg Msg, ctx MapContext) MappedCells {
	return MappedCells{}
}

func (bee *localBee) handleSlaveFailure(slaveID BeeID) {
	oldCol := bee.colony()
	newCol := oldCol.DeepCopy()
	if !newCol.DelSlave(slaveID) {
		return
	}

	glog.Warningf("Bee %v has a failed slave %v", bee.id(), slaveID)

	newCol.Generation++
	newCol, newSlaveIDs := bee.createSlavesForColony(newCol, 1)
	switch len(newSlaveIDs) {
	case 0:
		glog.Errorf("Cannot create a new slave for %v", newCol.Master)
	default:
		glog.V(2).Infof("Created slave %v for %v", newSlaveIDs[0], newCol.Master)
	}

	cells := bee.mappedCells()
	glog.V(2).Infof("Trying to replace %v with %v in the registry for %v", oldCol,
		newCol, cells)
	oldCol, err := bee.hive.registry.compareAndSet(oldCol, newCol, cells)
	if err != nil {
		glog.Errorf("Bee %v has an expired colony %v", bee.id(), newCol)
		bee.stop()
		return
	}

	bee.setColony(newCol)

	if len(newSlaveIDs) == 0 {
		return
	}

	glog.V(2).Infof("Successfully replaced the failed slave %v with %v", newCol,
		newSlaveIDs[0])
}

func (bee *localBee) handleMasterFailure(masterID BeeID) {
	oldCol := bee.colony()
	newCol := oldCol.DeepCopy()
	if !newCol.IsMaster(masterID) {
		return
	}

	if !newCol.DelSlave(bee.beeID) {
		return
	}

	glog.Warningf("Bee %v has a failed master %v", bee.id(), masterID)

	failedSlaves := make([]BeeID, 0, len(newCol.Slaves))
	slaveTxInfo := make(map[BeeID]TxInfo)
	for _, s := range newCol.Slaves {
		cmd := NewRemoteCmd(getTxInfoCmd{}, s)
		d, err := NewProxy(s.HiveID).SendCmd(&cmd)
		if err != nil {
			glog.V(2).Infof("Bee %v finds peer slave dead %v: %v", bee.id(), s, err)
			failedSlaves = append(failedSlaves, s)
			continue
		}

		info := d.(TxInfo)
		glog.V(2).Infof("Slave %v has this tx info %v", s, info)
		slaveTxInfo[s] = info
	}

	for s, info := range slaveTxInfo {
		if info.Generation > bee.gen() {
			glog.Errorf("Slave %v has an expired generation", s)
			bee.stop()
			return
		}
	}

	// If we can't find the cells of the colony, it's better just to stop this
	// process as soon as we can.
	cells, err := bee.hive.registry.mappedCells(oldCol)
	if err != nil {
		glog.Errorf("Cannot find the mapped cells of colony %v", oldCol)
		return
	}

	maxInfo := bee.getTxInfo()
	lastBufferedSlave := bee.id()
	for s, info := range slaveTxInfo {
		if info.Generation < maxInfo.Generation {
			continue
		}

		if info.LastCommitted > maxInfo.LastCommitted {
			maxInfo.LastCommitted = info.LastCommitted
		}

		if info.LastBuffered > maxInfo.LastBuffered {
			maxInfo.LastBuffered = info.LastBuffered
			lastBufferedSlave = s
		}
	}

	if maxInfo.LastCommitted > maxInfo.LastBuffered {
		glog.Errorf("Inconsistencies in slave state")
		// TODO(soheil): Maybe it's not a good thing to ignore such inconsistencies?
		// Should we stop the inconsistent bees?
		maxInfo.LastCommitted = maxInfo.LastBuffered
	}

	if lastBufferedSlave != bee.id() {
		cmd := RemoteCmd{
			Cmd: getTx{
				From: bee.txBuf[len(bee.txBuf)-1].Seq + 1,
				To:   maxInfo.LastBuffered,
			},
			CmdTo: lastBufferedSlave,
		}
		data, err := NewProxy(lastBufferedSlave.HiveID).SendCmd(&cmd)
		if err != nil {
			glog.Fatal("This part has not bee implemented yet.")
		}

		for _, tx := range data.([]Tx) {
			if tx.Seq <= maxInfo.LastCommitted {
				tx.Status = TxCommitted
			}
			bee.txBuf = append(bee.txBuf, tx)
		}
	}

	for s, info := range slaveTxInfo {
		if info.LastBuffered == maxInfo.LastBuffered {
			continue
		}

		var i int
		for i = len(bee.txBuf) - 1; i >= 0; i-- {
			if bee.txBuf[i].Seq == maxInfo.LastBuffered {
				break
			}
		}

		for ; i < len(bee.txBuf); i++ {
			cmd := RemoteCmd{
				Cmd: bufferTxCmd{
					Tx: bee.txBuf[i],
				},
				CmdTo: s,
			}
			_, err := NewProxy(s.HiveID).SendCmd(&cmd)
			if err != nil {
				glog.Fatal("This part has not bee implemented yet.")
			}
		}
	}

	for s, info := range slaveTxInfo {
		if info.LastCommitted == maxInfo.LastCommitted {
			continue
		}

		cmd := RemoteCmd{
			Cmd: commitTxCmd{
				Seq: maxInfo.LastCommitted,
			},
			CmdTo: s,
		}
		_, err := NewProxy(s.HiveID).SendCmd(&cmd)
		if err != nil {
			// FIXME(soheil): Handle failed bees.
			glog.Fatal("This part has not bee implemented yet: %v", err)
		}
	}

	nNewSlaves := bee.app.ReplicationFactor() - len(slaveTxInfo) - 1
	newCol, newSlaves := bee.createSlavesForColony(newCol, nNewSlaves)
	switch {
	case len(newSlaves) == 0:
		glog.Errorf("Cannot create a slave for colony %v: %v", newCol, err)
	case len(newSlaves) < bee.app.CommitThreshold():
		glog.Warningf("%v has %v slaves which is less than commit threshold of %v",
			newCol, len(newSlaves), bee.app.CommitThreshold())
	}

	newCol.Master = bee.beeID
	newCol.Generation++

	oldCol, err = bee.hive.registry.compareAndSet(oldCol, newCol, cells)
	if err != nil {
		glog.Errorf("Bee %#v has a expired colony %#v", bee.id(), newCol)
		bee.stop()
		return
	}

	bee.setColony(newCol)
	bee.addMappedCells(cells)

	for _, s := range newCol.Slaves {
		cmd := RemoteCmd{
			Cmd: joinColonyCmd{
				Colony: newCol,
			},
			CmdTo: s,
		}
		_, err := NewProxy(s.HiveID).SendCmd(&cmd)
		if err != nil {
			glog.Fatal("This part has not bee implemented yet.")
		}
	}

	bee.qee.lockLocally(bee, cells...)
	bee.commitAllBufferedTxs()
	bee.tx.Seq = maxInfo.LastBuffered

	//bee.add cells
	glog.V(2).Infof("Successfully replaced the failed master %v", newCol)
}

func (bee *localBee) createSlavesForColony(
	col BeeColony, nSlaves int) (BeeColony, []BeeID) {

	blacklist := col.SlaveHives()
	newCol := col.DeepCopy()
	newSlaves := make([]BeeID, 0, nSlaves)
	for {
		newSlaveHives := bee.hive.ReplicationStrategy().SelectSlaveHives(blacklist,
			nSlaves-len(newSlaves))
		if len(newSlaveHives) == 0 {
			return col, newSlaves
		}

		for _, h := range newSlaveHives {
			glog.V(2).Infof("Trying to create a slave bee on %v", h)
			newSlave, err := CreateBee(h, bee.app.Name())
			if err != nil {
				glog.V(2).Infof("Cannot create bee on %v: %v", h, err)
				blacklist = append(blacklist, newSlave.HiveID)
				continue
			}

			newCol.AddSlave(newSlave)
			if err = bee.qee.sendJoinColonyCmd(newCol, newSlave); err != nil {
				glog.Errorf("New slave %v cannot join the colony: %v", newSlave, err)
				newCol.DelSlave(newSlave)
				blacklist = append(blacklist, newSlave.HiveID)
				newCol.DelSlave(newSlave)
				continue
			}

			if err := bee.replicateAllTxOnSlave(newSlave); err != nil {
				glog.Errorf("Error in replicating on %v", newSlave)
				blacklist = append(blacklist, newSlave.HiveID)
				newCol.DelSlave(newSlave)
				continue
			}

			newSlaves = append(newSlaves, newSlave)
		}

		if len(newSlaves) < nSlaves {
			continue
		}

		return newCol, newSlaves
	}
}

func (bee *localBee) tryToRecruitSlaves() error {
	oldCol := bee.colony()
	if !bee.isMaster() {
		return fmt.Errorf("%v is not the master of %v", bee.id(), oldCol)
	}

	nSlaves := bee.app.ReplicationFactor() - len(oldCol.Slaves) - 1
	if nSlaves <= 0 {
		return nil
	}

	newCol, newSlaves := bee.createSlavesForColony(oldCol.DeepCopy(), nSlaves)
	glog.V(2).Infof("Recruited slaves %v for %v", newSlaves, oldCol)

	for _, s := range newCol.Slaves {
		cmd := RemoteCmd{
			Cmd: joinColonyCmd{
				Colony: newCol,
			},
			CmdTo: s,
		}
		_, err := NewProxy(s.HiveID).SendCmd(&cmd)
		if err != nil {
			glog.Errorf("Slave %v didn't join %v: %v", s, newCol, err)
		}

		newCol.DelSlave(s)
	}

	cells := bee.mappedCells()
	_, err := bee.hive.registry.compareAndSet(oldCol, newCol, cells)
	if err != nil {
		return err
	}

	bee.setColony(newCol)

	if len(newCol.Slaves) < bee.app.CommitThreshold() {
		return fmt.Errorf(
			"%v has %v slaves which is lower than commmit threshold of %v",
			bee.id(), len(newCol.Slaves), bee.app.CommitThreshold())
	}

	return nil
}