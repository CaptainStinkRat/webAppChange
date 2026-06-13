"""Mock FastAPI backend — task manager API."""

from fastapi import FastAPI, HTTPException, Query
from pydantic import BaseModel
import uvicorn

# ── Models ──────────────────────────────────────────────

class TaskCreate(BaseModel):
    title: str
    completed: bool = False

class Task(BaseModel):
    id: int
    title: str
    completed: bool

# ── In-memory store ─────────────────────────────────────

tasks: dict[int, Task] = {}
next_id: int = 1

# ── App ─────────────────────────────────────────────────

app = FastAPI(title="Task Manager API")

@app.get("/api/tasks")
def list_tasks(completed: bool | None = Query(None)):
    """Return all tasks, optionally filtered by completion status."""
    if completed is None:
        return list(tasks.values())
    return [t for t in tasks.values() if t.completed == completed]

@app.post("/api/tasks", status_code=201)
def create_task(body: TaskCreate):
    global next_id
    task = Task(id=next_id, title=body.title, completed=body.completed)
    tasks[next_id] = task
    next_id += 1
    return task

@app.get("/api/tasks/{task_id}")
def get_task(task_id: int):
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="Task not found")
    return tasks[task_id]

if __name__ == "__main__":
    uvicorn.run("main:app", host="0.0.0.0", port=8000, reload=True)
