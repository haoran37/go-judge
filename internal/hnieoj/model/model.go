package model

import "time"

const (
	StatusPending             = -10
	StatusCompiling           = -9
	StatusRunning             = -8
	StatusAccepted            = 0
	StatusRuntimeError        = 1
	StatusCompileError        = 2
	StatusWrongAnswer         = 3
	StatusTimeLimitExceeded   = 4
	StatusMemoryLimitExceeded = 5
	StatusSystemError         = 6
	StatusJudgementFailed     = 7
	StatusInvalidInteraction  = 8
)

const (
	EventSubmissionCreated = "SUBMISSION_CREATED"
	EventStatusChanged     = "STATUS_CHANGED"
	EventCaseFinished      = "CASE_FINISHED"
	EventJudgeFinished     = "JUDGE_FINISHED"
	EventJudgeFailed       = "JUDGE_FAILED"
)

const (
	ProblemTypeACM = 0
	ProblemTypeOI  = 1
)

const (
	JudgeModeDefault     = "default"
	JudgeModeSPJ         = "spj"
	JudgeModeInteractive = "interactive"
)

type Task struct {
	SubmissionID     string             `json:"submissionId"`
	JudgeTaskID      string             `json:"judgeTaskId"`
	JudgeID          int64              `json:"judgeId"`
	ProblemID        int64              `json:"problemId"`
	ProblemCode      string             `json:"problemCode"`
	Language         string             `json:"language"`
	Code             string             `json:"code"`
	TimeLimit        int64              `json:"timeLimit"`
	MemoryLimit      int64              `json:"memoryLimit"`
	StackLimit       int64              `json:"stackLimit"`
	JudgeMode        string             `json:"judgeMode"`
	Checker          *Program           `json:"checker,omitempty"`
	Interactor       *Program           `json:"interactor,omitempty"`
	Interaction      *InteractionConfig `json:"interaction,omitempty"`
	ProblemType      int                `json:"problemType"`
	IOScore          int                `json:"ioScore"`
	IsRemoveEndBlank bool               `json:"isRemoveEndBlank"`
	DataVersion      int64              `json:"dataVersion"`
	ContestID        int64              `json:"contestId"`
	CreatedAt        time.Time          `json:"createdAt"`
}

type Program struct {
	Language         string `json:"language"`
	Source           string `json:"source"`
	ArtifactFileID   string `json:"artifactFileId"`
	TimeLimit        int64  `json:"timeLimit"`
	MemoryLimit      int64  `json:"memoryLimit"`
	StackLimit       int64  `json:"stackLimit"`
	OutputLimit      int64  `json:"outputLimit"`
	Protocol         string `json:"protocol"`
	ArgumentTemplate string `json:"argumentTemplate"`
}

type InteractionConfig struct {
	Protocol  string `json:"protocol"`
	Wiring    string `json:"wiring"`
	ScoreMode string `json:"scoreMode"`
}

type Case struct {
	ID       string
	Input    string
	Expected string
}

type CaseResult struct {
	CaseID            string `json:"caseId"`
	Status            int    `json:"status"`
	StatusText        string `json:"statusText"`
	Time              int64  `json:"time"`
	Memory            int64  `json:"memory"`
	Score             int    `json:"score"`
	UserOutput        string `json:"userOutput,omitempty"`
	DiagnosticMessage string `json:"diagnosticMessage,omitempty"`
}

type Event struct {
	EventType         string      `json:"eventType"`
	SubmissionID      string      `json:"submissionId"`
	JudgeTaskID       string      `json:"judgeTaskId"`
	Status            int         `json:"status"`
	StatusText        string      `json:"statusText"`
	TotalCase         int         `json:"totalCase"`
	JudgedCase        int         `json:"judgedCase"`
	CurrentCase       int         `json:"currentCase"`
	Score             int         `json:"score,omitempty"`
	CaseResult        *CaseResult `json:"caseResult,omitempty"`
	Message           string      `json:"message"`
	DiagnosticMessage string      `json:"diagnosticMessage,omitempty"`
	EventTime         time.Time   `json:"eventTime"`
}

func StatusText(status int) string {
	switch status {
	case StatusPending:
		return "Pending"
	case StatusCompiling:
		return "Compiling"
	case StatusRunning:
		return "Running"
	case StatusAccepted:
		return "Accepted"
	case StatusRuntimeError:
		return "Runtime Error"
	case StatusCompileError:
		return "Compile Error"
	case StatusWrongAnswer:
		return "Wrong Answer"
	case StatusTimeLimitExceeded:
		return "Time Limit Exceeded"
	case StatusMemoryLimitExceeded:
		return "Memory Limit Exceeded"
	case StatusSystemError:
		return "System Error"
	case StatusJudgementFailed:
		return "Judgement Failed"
	case StatusInvalidInteraction:
		return "Invalid Interaction"
	default:
		return "Unknown"
	}
}

func NewEvent(eventType string, task Task, status, totalCase, judgedCase, currentCase int, caseResult *CaseResult, message string) Event {
	return Event{
		EventType:    eventType,
		SubmissionID: task.SubmissionID,
		JudgeTaskID:  task.JudgeTaskID,
		Status:       status,
		StatusText:   StatusText(status),
		TotalCase:    totalCase,
		JudgedCase:   judgedCase,
		CurrentCase:  currentCase,
		CaseResult:   caseResult,
		Message:      message,
		EventTime:    time.Now(),
	}
}
