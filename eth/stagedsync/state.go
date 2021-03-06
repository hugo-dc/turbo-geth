package stagedsync

import (
	"fmt"

	"github.com/ledgerwatch/turbo-geth/eth/stagedsync/stages"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/log"
)

type State struct {
	unwindStack  *PersistentUnwindStack
	stages       []*Stage
	currentStage uint
}

func (s *State) Len() int {
	return len(s.stages)
}

func (s *State) NextStage() {
	if s == nil {
		return
	}
	s.currentStage++
}

func (s *State) GetLocalHeight(db ethdb.Getter) (uint64, error) {
	state, err := s.StageState(stages.Headers, db)
	return state.BlockNumber, err
}

func (s *State) UnwindTo(blockNumber uint64, db ethdb.Database) error {
	log.Info("UnwindTo", "block", blockNumber)
	for _, stage := range s.stages {
		if err := s.unwindStack.Add(UnwindState{stage.ID, blockNumber, nil}, db); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) IsDone() bool {
	return s.currentStage >= uint(len(s.stages)) && s.unwindStack.Empty()
}

func (s *State) CurrentStage() (uint, *Stage) {
	return s.currentStage, s.stages[s.currentStage]
}

func (s *State) SetCurrentStage(id stages.SyncStage) error {
	for i, stage := range s.stages {
		if stage.ID == id {
			s.currentStage = uint(i)
			return nil
		}
	}
	return fmt.Errorf("stage not found with id: %v", id)
}

func (s *State) StageByID(id stages.SyncStage) (*Stage, error) {
	for _, stage := range s.stages {
		if stage.ID == id {
			return stage, nil
		}
	}
	return nil, fmt.Errorf("stage not found with id: %v", id)
}

func NewState(stages []*Stage) *State {
	return &State{
		stages:       stages,
		currentStage: 0,
		unwindStack:  NewPersistentUnwindStack(),
	}
}

func (s *State) LoadUnwindInfo(db ethdb.Getter) error {
	for _, stage := range s.stages {
		if err := s.unwindStack.AddFromDB(db, stage.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) StageState(stage stages.SyncStage, db ethdb.Getter) (*StageState, error) {
	blockNum, stageData, err := stages.GetStageProgress(db, stage)
	if err != nil {
		return nil, err
	}
	return &StageState{s, stage, blockNum, stageData}, nil
}

func (s *State) findInterruptedStage(db ethdb.Getter) (*Stage, error) {
	for _, stage := range s.stages {
		_, stageData, err := stages.GetStageProgress(db, stage.ID)
		if err != nil {
			return nil, err
		}
		if len(stageData) > 0 {
			return stage, nil
		}
	}
	return nil, nil
}

func (s *State) findInterruptedUnwindStage(db ethdb.Getter) (*Stage, error) {
	for _, stage := range s.stages {
		_, stageData, err := stages.GetStageUnwind(db, stage.ID)
		if err != nil {
			return nil, err
		}
		if len(stageData) > 0 {
			return stage, nil
		}
	}
	return nil, nil
}

func (s *State) RunInterruptedStage(db ethdb.GetterPutter) error {
	if interruptedStage, err := s.findInterruptedStage(db); err != nil {
		return err
	} else if interruptedStage != nil {
		if err := s.runStage(interruptedStage, db); err != nil {
			return err
		}
		// restart from 0 after completing the missing stage
		s.currentStage = 0
	}

	if interruptedStage, err := s.findInterruptedUnwindStage(db); err != nil {
		return err
	} else if interruptedStage != nil {
		u, err := s.unwindStack.LoadFromDB(db, interruptedStage.ID)
		if err != nil {
			return err
		}
		if u == nil {
			return nil
		}
		return s.UnwindStage(u, db)
	}
	return nil
}

func (s *State) Run(db ethdb.GetterPutter) error {
	if err := s.RunInterruptedStage(db); err != nil {
		return err
	}
	for !s.IsDone() {
		if !s.unwindStack.Empty() {
			for unwind := s.unwindStack.Pop(); unwind != nil; unwind = s.unwindStack.Pop() {
				if err := s.UnwindStage(unwind, db); err != nil {
					return err
				}
			}
			return nil
		}

		index, stage := s.CurrentStage()

		if stage.Disabled {
			message := fmt.Sprintf(
				"Sync stage %d/%d. %v disabled. %s",
				index+1,
				s.Len(),
				stage.Description,
				stage.DisabledDescription,
			)

			log.Info(message)

			s.NextStage()
			continue
		}

		if err := s.runStage(stage, db); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) RunStage(id stages.SyncStage, db ethdb.Getter) error {
	stage, err := s.StageByID(id)
	if err != nil {
		return err
	}
	return s.runStage(stage, db)
}

func (s *State) runStage(stage *Stage, db ethdb.Getter) error {
	stageState, err := s.StageState(stage.ID, db)
	if err != nil {
		return err
	}

	message := fmt.Sprintf("Sync stage %d/%d. %v...", stage.ID+1, s.Len(), stage.Description)
	log.Info(message)

	err = stage.ExecFunc(stageState, s)
	if err != nil {
		return err
	}

	log.Info(fmt.Sprintf("%s DONE!", message))
	return nil
}

func (s *State) UnwindStage(unwind *UnwindState, db ethdb.GetterPutter) error {
	log.Info("Unwinding...", "stage", unwind.Stage)
	stage, err := s.StageByID(unwind.Stage)
	if err != nil {
		return err
	}
	if stage.UnwindFunc == nil {
		return nil
	}

	stageState, err := s.StageState(unwind.Stage, db)
	if err != nil {
		return err
	}

	if stageState.BlockNumber <= unwind.UnwindPoint {
		if err = unwind.Skip(db); err != nil {
			return err
		}
		return nil
	}

	err = stage.UnwindFunc(unwind, stageState)
	if err != nil {
		return err
	}

	// always restart from stage 1 after unwind
	s.currentStage = 0
	log.Info("Unwinding... DONE!")
	return nil
}
