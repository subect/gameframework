package game

import "sync"

// 输入类型常量
const (
	InputNone  = 0
	InputLeft  = 1
	InputRight = 2
	InputUp    = 3
	InputDown  = 4
)

// PlayerPos 玩家位置（2D）
type PlayerPos struct {
	X, Y float32
}

// GameState 保存玩家的2D位置
type GameState struct {
	mu  sync.Mutex
	Pos map[uint16]*PlayerPos
}

func NewGameState() *GameState {
	return &GameState{Pos: make(map[uint16]*PlayerPos)}
}

// ApplyInput 对某玩家的输入应用到 state（2D移动）
func (g *GameState) ApplyInput(pid uint16, input uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.Pos[pid]; !ok {
		g.Pos[pid] = &PlayerPos{X: 0.0, Y: 0.0}
	}
	pos := g.Pos[pid]
	// 为兼容 60Hz 帧率，将速度调整为 0.5（与 20Hz、1.5 速度等效位移）
	speed := float32(0.5)
	switch input {
	case InputLeft:
		pos.X -= speed
	case InputRight:
		pos.X += speed
	case InputUp:
		pos.Y += speed
	case InputDown:
		pos.Y -= speed
	}
}

// GetPos 获取玩家位置
func (g *GameState) GetPos(pid uint16) *PlayerPos {
	g.mu.Lock()
	defer g.mu.Unlock()
	if pos, ok := g.Pos[pid]; ok {
		return &PlayerPos{X: pos.X, Y: pos.Y}
	}
	return &PlayerPos{X: 0.0, Y: 0.0}
}

// GetAllPositions 获取所有玩家位置
func (g *GameState) GetAllPositions() map[uint16]*PlayerPos {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make(map[uint16]*PlayerPos)
	for pid, pos := range g.Pos {
		result[pid] = &PlayerPos{X: pos.X, Y: pos.Y}
	}
	return result
}
