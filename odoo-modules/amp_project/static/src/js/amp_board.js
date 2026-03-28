/** @odoo-module **/
/**
 * AMP Live Board — OWL component
 * JIRA-style kanban: columns = task states, rows = stories within each epic.
 * Subscribes to bus.bus channel "amp_board_{project_id}" for instant updates.
 */

import { Component, useState, onWillStart, onMounted, onWillUnmount } from "@odoo/owl";
import { registry } from "@web/core/registry";
import { useService } from "@web/core/utils/hooks";
import { rpc } from "@web/core/network/rpc";

// ── Constants ─────────────────────────────────────────────────────────────────

const STATES = [
    { key: "backlog",     label: "Backlog",     color: "#6b7280" },
    { key: "ready",       label: "Ready",       color: "#3b82f6" },
    { key: "in_progress", label: "In Progress", color: "#8b5cf6" },
    { key: "review",      label: "Review",      color: "#f59e0b" },
    { key: "completed",   label: "Completed",   color: "#10b981" },
    { key: "blocked",     label: "Blocked",     color: "#ef4444" },
];

const PRIORITY_LABELS = { "0": "Low", "1": "Medium", "2": "High", "3": "Critical" };
const PRIORITY_COLORS = { "0": "#9ca3af", "1": "#60a5fa", "2": "#f97316", "3": "#dc2626" };

// ── TaskCard sub-component ────────────────────────────────────────────────────
// Self-contained: owns its own action service so the click handler
// never has a 'this' scoping problem when called from a template prop.

class TaskCard extends Component {
    static template = "amp_project.TaskCard";
    static props = ["task"];

    setup() {
        this.actionService = useService("action");
    }

    get priorityLabel() {
        return PRIORITY_LABELS[this.props.task.priority] || "Medium";
    }
    get priorityColor() {
        return PRIORITY_COLORS[this.props.task.priority] || "#60a5fa";
    }
    get agentInitials() {
        const a = this.props.task.agent_id;
        if (!a) return "";
        return a.split("-").map(p => p[0]).join("").toUpperCase().slice(0, 2);
    }

    openTask() {
        this.actionService.doAction({
            type: "ir.actions.act_window",
            res_model: "amp.task",
            res_id: this.props.task.id,
            views: [[false, "form"]],
            target: "new",
        });
    }
}

// ── Main AmpBoard component ───────────────────────────────────────────────────

class AmpBoard extends Component {
    static template = "amp_project.AmpBoard";
    static components = { TaskCard };

    setup() {
        this.busService = useService("bus_service");
        this.actionService = useService("action");
        this.notification = useService("notification");

        this.state = useState({
            projects: [],
            selectedProjectId: null,
            project: null,
            epics: [],
            loading: false,
            error: null,
            filter: {
                epicId: null,
                agentId: null,
                search: "",
                showCompleted: true,
            },
        });

        this._busChannel = null;
        this._refreshTimeout = null;

        onWillStart(async () => {
            await this._loadProjects();
        });

        onMounted(() => {
            if (this.state.projects.length === 1) {
                this._selectProject(this.state.projects[0].id);
            }
        });

        onWillUnmount(() => {
            this._unsubscribeBus();
        });
    }

    // ── Data loading ───────────────────────────────────────────────────────────

    async _loadProjects() {
        try {
            const projects = await rpc("/amp/board/projects", {});
            this.state.projects = projects;
        } catch (e) {
            this.state.error = "Failed to load projects.";
        }
    }

    async _selectProject(projectId) {
        if (this.state.selectedProjectId === projectId) return;
        this._unsubscribeBus();
        this.state.selectedProjectId = projectId;
        this.state.filter.epicId = null;
        this.state.filter.agentId = null;
        this.state.filter.search = "";
        await this._loadBoard();
        this._subscribeBus(projectId);
    }

    async _loadBoard() {
        const pid = this.state.selectedProjectId;
        if (!pid) return;
        this.state.loading = true;
        this.state.error = null;
        try {
            const data = await rpc(`/amp/board/${pid}/data`, {});
            if (data && data.error) {
                this.state.error = data.error;
            } else if (data) {
                this.state.project = data.project;
                this.state.epics = data.epics;
            }
        } catch (e) {
            this.state.error = "Failed to load board data.";
        } finally {
            this.state.loading = false;
        }
    }

    // ── Bus real-time subscription ─────────────────────────────────────────────

    _subscribeBus(projectId) {
        const channel = `amp_board_${projectId}`;
        this.busService.subscribe("amp_task_update", (payload) => this._onBusMessage(payload));
        this.busService.addChannel(channel);
        this._busChannel = channel;
    }

    _unsubscribeBus() {
        if (this._busChannel) {
            this.busService.deleteChannel(this._busChannel);
            this._busChannel = null;
        }
    }

    _onBusMessage(payload) {
        if (!payload) return;
        const taskId = payload.task_id;

        let found = false;
        for (const epic of this.state.epics) {
            for (const story of epic.stories) {
                const idx = story.tasks.findIndex(t => t.id === taskId);
                if (idx !== -1) {
                    Object.assign(story.tasks[idx], {
                        state: payload.state,
                        agent_id: payload.agent_id || story.tasks[idx].agent_id,
                        is_ready: payload.is_ready,
                        dag_critical_path: payload.dag_critical_path,
                        _flash: true,
                    });
                    found = true;
                    this._refreshHeaderCounts();
                    setTimeout(() => { story.tasks[idx]._flash = false; }, 1200);
                    break;
                }
            }
            if (found) break;
        }

        if (!found) {
            clearTimeout(this._refreshTimeout);
            this._refreshTimeout = setTimeout(() => this._loadBoard(), 800);
        }
    }

    _refreshHeaderCounts() {
        if (!this.state.project) return;
        let total = 0, completed = 0, blocked = 0;
        const agents = new Set();
        for (const epic of this.state.epics) {
            for (const story of epic.stories) {
                for (const task of story.tasks) {
                    total++;
                    if (task.state === "completed") completed++;
                    if (task.state === "blocked") blocked++;
                    if (task.agent_id) agents.add(task.agent_id);
                }
            }
        }
        this.state.project.task_count = total;
        this.state.project.completed_task_count = completed;
        this.state.project.blocked_task_count = blocked;
        this.state.project.active_agent_count = agents.size;
        this.state.project.progress_percentage =
            total > 0 ? Math.round((completed / total) * 1000) / 10 : 0;
    }

    // ── Computed getters ───────────────────────────────────────────────────────

    get states() { return STATES; }

    get visibleEpics() {
        const { epicId } = this.state.filter;
        return epicId
            ? this.state.epics.filter(e => e.id === epicId)
            : this.state.epics;
    }

    get uniqueAgents() {
        const agents = new Set();
        for (const epic of this.state.epics) {
            for (const story of epic.stories) {
                for (const task of story.tasks) {
                    if (task.agent_id) agents.add(task.agent_id);
                }
            }
        }
        return [...agents].sort();
    }

    tasksForColumn(story, stateKey) {
        const { agentId, search, showCompleted } = this.state.filter;
        if (stateKey === "completed" && !showCompleted) return [];
        return story.tasks.filter(t => {
            if (t.state !== stateKey) return false;
            if (agentId && t.agent_id !== agentId) return false;
            if (search) {
                const q = search.toLowerCase();
                if (!t.name.toLowerCase().includes(q)) return false;
            }
            return true;
        });
    }

    columnCount(stateKey) {
        let count = 0;
        for (const epic of this.visibleEpics) {
            for (const story of epic.stories) {
                count += this.tasksForColumn(story, stateKey).length;
            }
        }
        return count;
    }

    get progressBarWidth() {
        return `${this.state.project?.progress_percentage || 0}%`;
    }

    // ── Event handlers ─────────────────────────────────────────────────────────

    onProjectClick(projectId) {
        this._selectProject(projectId);
    }

    onEpicFilter(ev) {
        const val = ev.target.value;
        this.state.filter.epicId = val ? parseInt(val) : null;
    }

    onAgentFilter(ev) {
        this.state.filter.agentId = ev.target.value || null;
    }

    onSearchInput(ev) {
        this.state.filter.search = ev.target.value;
    }

    onToggleCompleted() {
        this.state.filter.showCompleted = !this.state.filter.showCompleted;
    }

    onRefresh() {
        this._loadBoard();
    }
}

// ── Register as client action ─────────────────────────────────────────────────

registry.category("actions").add("amp_board_action", AmpBoard);
