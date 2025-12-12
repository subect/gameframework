package game

import "sync"

// GameState 保存简单的位置（按 playerID）
type GameState struct {
	mu  sync.Mutex
	Pos map[uint16]float32
}

func NewGameState() *GameState {
	return &GameState{Pos: make(map[uint16]float32)}
}

// ApplyInput 对某玩家的输入应用到 state（简单位移）
func (g *GameState) ApplyInput(pid uint16, input uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.Pos[pid]; !ok {
		g.Pos[pid] = 0.0
	}
	if input == 1 {
		g.Pos[pid] -= 0.1
	} else if input == 2 {
		g.Pos[pid] += 0.1
	}
}
func (g *GameState) GetPos(pid uint16) float32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Pos[pid]
}
