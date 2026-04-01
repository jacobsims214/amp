# Directed Acyclic Graph (DAG) Skill

A **DAG** (Directed Acyclic Graph) is a graph with directed edges and no cycles. Perfect for modeling task dependencies and workflows.

## Core Concepts

- **Node/Vertex**: A task or step in the workflow
- **Directed Edge**: Dependency relationship (A → B means "B depends on A")
- **Acyclic**: No circular dependencies allowed
- **Topological Order**: Execution order where dependencies complete before dependents

## Key Properties

1. **No cycles**: Cannot have A → B → C → A
2. **Partial order**: Some tasks can run in parallel
3. **Sources**: Nodes with no incoming edges (can start immediately)
4. **Sinks**: Nodes with no outgoing edges (final tasks)

## Python Implementation

```python
from typing import Set, Dict, List, Optional
from collections import deque, defaultdict

class DAG:
    """Directed Acyclic Graph for task dependency management"""
    
    def __init__(self):
        self.nodes: Set[str] = set()
        self.edges: Dict[str, Set[str]] = defaultdict(set)  # node -> its dependents
        self.dependencies: Dict[str, Set[str]] = defaultdict(set)  # node -> its dependencies
    
    def add_node(self, node_id: str) -> None:
        """Add a node to the graph"""
        self.nodes.add(node_id)
    
    def add_edge(self, from_node: str, to_node: str) -> None:
        """Add dependency: from_node must complete before to_node"""
        # Check for cycles before adding
        if self._would_create_cycle(from_node, to_node):
            raise ValueError(f"Adding edge {from_node} -> {to_node} would create a cycle")
        
        self.add_node(from_node)
        self.add_node(to_node)
        self.edges[from_node].add(to_node)
        self.dependencies[to_node].add(from_node)
    
    def _would_create_cycle(self, from_node: str, to_node: str) -> bool:
        """Check if adding edge would create a cycle"""
        # If to_node can already reach from_node, adding from_node -> to_node creates cycle
        return self._can_reach(to_node, from_node, set())
    
    def _can_reach(self, start: str, target: str, visited: Set[str]) -> bool:
        """DFS to check if target is reachable from start"""
        if start == target:
            return True
        if start in visited:
            return False
        visited.add(start)
        for neighbor in self.edges[start]:
            if self._can_reach(neighbor, target, visited):
                return True
        return False
    
    def get_dependencies(self, node: str) -> Set[str]:
        """Get all nodes that must complete before this node"""
        return self.dependencies[node].copy()
    
    def get_dependents(self, node: str) -> Set[str]:
        """Get all nodes that depend on this node"""
        return self.edges[node].copy()
    
    def topological_sort(self) -> List[str]:
        """
        Return nodes in topological order (Kahn's algorithm).
        Dependencies appear before dependents.
        """
        # Calculate in-degrees
        in_degree = {node: 0 for node in self.nodes}
        for node in self.nodes:
            for dependent in self.edges[node]:
                in_degree[dependent] += 1
        
        # Start with nodes having no dependencies
        queue = deque([node for node in self.nodes if in_degree[node] == 0])
        result = []
        
        while queue:
            node = queue.popleft()
            result.append(node)
            
            # Reduce in-degree of dependents
            for dependent in self.edges[node]:
                in_degree[dependent] -= 1
                if in_degree[dependent] == 0:
                    queue.append(dependent)
        
        if len(result) != len(self.nodes):
            raise ValueError("Graph contains cycles")
        
        return result
    
    def get_ready_nodes(self, completed: Set[str]) -> Set[str]:
        """Get nodes ready to execute (all dependencies completed)"""
        ready = set()
        for node in self.nodes:
            if node not in completed:
                if self.dependencies[node].issubset(completed):
                    ready.add(node)
        return ready
    
    def get_all_ancestors(self, node: str) -> Set[str]:
        """Get all transitive dependencies of a node"""
        ancestors = set()
        stack = list(self.dependencies[node])
        while stack:
            current = stack.pop()
            if current not in ancestors:
                ancestors.add(current)
                stack.extend(self.dependencies[current])
        return ancestors
    
    def get_all_descendants(self, node: str) -> Set[str]:
        """Get all nodes that transitively depend on this node"""
        descendants = set()
        stack = list(self.edges[node])
        while stack:
            current = stack.pop()
            if current not in descendants:
                descendants.add(current)
                stack.extend(self.edges[current])
        return descendants
    
    def is_valid(self) -> bool:
        """Check if graph has no cycles"""
        try:
            self.topological_sort()
            return True
        except ValueError:
            return False
    
    def to_dict(self) -> Dict:
        """Serialize to dictionary"""
        return {
            'nodes': list(self.nodes),
            'edges': {k: list(v) for k, v in self.edges.items()}
        }
    
    @classmethod
    def from_dict(cls, data: Dict) -> 'DAG':
        """Deserialize from dictionary"""
        dag = cls()
        for node in data.get('nodes', []):
            dag.add_node(node)
        for from_node, to_nodes in data.get('edges', {}).items():
            for to_node in to_nodes:
                dag.add_edge(from_node, to_node)
        return dag


class TaskDAG:
    """DAG specifically for task/workflow management"""
    
    def __init__(self):
        self.dag = DAG()
        self.task_data: Dict[str, Dict] = {}
    
    def add_task(self, task_id: str, data: Optional[Dict] = None) -> None:
        """Add a task to the workflow"""
        self.dag.add_node(task_id)
        self.task_data[task_id] = data or {}
    
    def add_dependency(self, prerequisite: str, dependent: str) -> None:
        """
        Add dependency: prerequisite must complete before dependent can start.
        Edge direction: prerequisite -> dependent
        """
        self.dag.add_edge(prerequisite, dependent)
    
    def get_execution_order(self) -> List[str]:
        """Get tasks in execution order (topological sort)"""
        return self.dag.topological_sort()
    
    def get_parallel_groups(self) -> List[Set[str]]:
        """
        Group tasks that can run in parallel.
        Returns list of sets, each set is a parallelizable group.
        """
        in_degree = {node: 0 for node in self.dag.nodes}
        for node in self.dag.nodes:
            for dependent in self.dag.edges[node]:
                in_degree[dependent] += 1
        
        groups = []
        remaining = set(self.dag.nodes)
        completed = set()
        
        while remaining:
            # Find all nodes with no remaining dependencies
            ready = {node for node in remaining 
                     if self.dag.dependencies[node].issubset(completed)}
            if not ready:
                raise ValueError("Cycle detected or invalid state")
            
            groups.append(ready)
            completed.update(ready)
            remaining -= ready
        
        return groups
    
    def what_blocks(self, task_id: str) -> Set[str]:
        """What tasks are blocking this task from starting?"""
        return self.dag.get_dependencies(task_id)
    
    def what_unlocks(self, task_id: str) -> Set[str]:
        """What tasks become unblocked when this task completes?"""
        return self.dag.get_dependents(task_id)
    
    def get_critical_path(self) -> List[str]:
        """
        Find the longest path from any source to any sink.
        These tasks dictate minimum completion time.
        """
        # Simple version: assume all edges have weight 1
        topo_order = self.dag.topological_sort()
        
        # Distance from sources
        dist = {node: 0 for node in self.dag.nodes}
        predecessor = {node: None for node in self.dag.nodes}
        
        for node in topo_order:
            for dependent in self.dag.edges[node]:
                if dist[node] + 1 > dist[dependent]:
                    dist[dependent] = dist[node] + 1
                    predecessor[dependent] = node
        
        # Find sink with max distance
        max_dist = -1
        end_node = None
        for node in self.dag.nodes:
            if not self.dag.edges[node]:  # is sink
                if dist[node] > max_dist:
                    max_dist = dist[node]
                    end_node = node
        
        # Reconstruct path
        path = []
        current = end_node
        while current is not None:
            path.append(current)
            current = predecessor[current]
        
        return list(reversed(path))
```

## Usage Examples

### Basic Dependency Chain

```python
dag = TaskDAG()

# Setup: A -> B -> C
dag.add_task("setup-env")
dag.add_task("install-deps")
dag.add_task("run-tests")

dag.add_dependency("setup-env", "install-deps")
dag.add_dependency("install-deps", "run-tests")

# Execution order
order = dag.get_execution_order()
# ['setup-env', 'install-deps', 'run-tests']

# What blocks 'run-tests'?
blocking = dag.what_blocks("run-tests")
# {'install-deps'}
```

### Parallel Execution Groups

```python
dag = TaskDAG()

# Complex workflow:
# A -> C -> E
# B -> C -> E
# D -> E

dag.add_task("A")
dag.add_task("B")
dag.add_task("C")
dag.add_task("D")
dag.add_task("E")

dag.add_dependency("A", "C")
dag.add_dependency("B", "C")
dag.add_dependency("C", "E")
dag.add_dependency("D", "E")

# Parallel groups
groups = dag.get_parallel_groups()
# [{'A', 'B', 'D'}, {'C'}, {'E'}]
# First group: A, B, D can run in parallel
# Second group: C runs after A and B complete
# Third group: E runs after C and D complete
```

### AMP Workflow with Odoo

```python
# Creating epic/story/task DAG in Odoo
def create_amp_workflow(project_id: int, epic_name: str):
    dag = TaskDAG()
    
    # Epic with multiple stories that can run in parallel
    dag.add_task(f"epic:{epic_name}", {"type": "epic", "project_id": project_id})
    
    # Stories under epic
    stories = ["auth", "database", "api"]
    for story in stories:
        dag.add_task(f"story:{story}", {"type": "story"})
        dag.add_dependency(f"epic:{epic_name}", f"story:{story}")
        
        # Tasks under each story
        for i in range(2):
            task_id = f"task:{story}:{i}"
            dag.add_task(task_id, {"type": "task"})
            dag.add_dependency(f"story:{story}", task_id)
    
    # Final integration task depends on all stories
    dag.add_task("task:integration", {"type": "task"})
    for story in stories:
        dag.add_dependency(f"story:{story}", "task:integration")
    
    return dag.get_execution_order()
```

## Key Algorithms

### Kahn's Algorithm (Topological Sort)

```python
def topological_sort_kahn(graph):
    """
    1. Calculate in-degree of each node
    2. Start with nodes having in-degree 0
    3. Process nodes, reducing in-degree of neighbors
    4. When neighbor's in-degree reaches 0, add to queue
    5. Repeat until all nodes processed
    """
    in_degree = {node: 0 for node in graph.nodes}
    for edges in graph.edges.values():
        for to_node in edges:
            in_degree[to_node] += 1
    
    queue = deque([n for n in graph.nodes if in_degree[n] == 0])
    result = []
    
    while queue:
        node = queue.popleft()
        result.append(node)
        for neighbor in graph.edges[node]:
            in_degree[neighbor] -= 1
            if in_degree[neighbor] == 0:
                queue.append(neighbor)
    
    return result
```

### DFS-based Cycle Detection

```python
def has_cycle_dfs(graph):
    """
    States: 0=unvisited, 1=visiting, 2=visited
    If we encounter a node in 'visiting' state, we found a cycle
    """
    WHITE, GRAY, BLACK = 0, 1, 2
    color = {node: WHITE for node in graph.nodes}
    
    def dfs(node):
        color[node] = GRAY
        for neighbor in graph.edges[node]:
            if color[neighbor] == GRAY:
                return True  # Back edge found = cycle
            if color[neighbor] == WHITE and dfs(neighbor):
                return True
        color[node] = BLACK
        return False
    
    for node in graph.nodes:
        if color[node] == WHITE:
            if dfs(node):
                return True
    return False
```

## Common Patterns

### Pattern 1: Fan-out/Fan-in
```
    A
   /|\
  B C D
   \|/
    E
```
A triggers B, C, D in parallel. E waits for all.

### Pattern 2: Pipeline
```
A -> B -> C -> D
```
Sequential processing, output of one is input to next.

### Pattern 3: Diamond
```
  A
 / \
B   C
 \ /
  D
```
A branches to B and C, then converges at D.

### Pattern 4: Conditional
```
A -> B? -> C
  \-> D
```
A always runs, then either B→C or D based on condition.

## References

- Wikipedia: https://en.wikipedia.org/wiki/Directed_acyclic_graph
- Topological Sorting: https://en.wikipedia.org/wiki/Topological_sorting
- Kahn's Algorithm: Kahn, Arthur B. (1962), "Topological sorting of large networks"
