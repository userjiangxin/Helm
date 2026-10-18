import { NavLink, Route, Routes } from "react-router-dom";
import { useLiveUpdates } from "./live";
import { Board } from "./pages/Board";
import { Machines } from "./pages/Machines";
import { TaskPage } from "./pages/TaskPage";

function Mark() {
  // A helm wheel reduced to its hub and spokes.
  return (
    <svg width="20" height="20" viewBox="0 0 20 20" aria-hidden="true">
      <circle cx="10" cy="10" r="6.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <circle cx="10" cy="10" r="2" fill="currentColor" />
      {[0, 45, 90, 135].map((a) => (
        <line key={a} x1="10" y1="0.8" x2="10" y2="19.2" stroke="currentColor" strokeWidth="1.6" transform={`rotate(${a} 10 10)`} />
      ))}
    </svg>
  );
}

export function App() {
  const live = useLiveUpdates();
  return (
    <div className="shell">
      <header className="topbar">
        <NavLink to="/" className="wordmark"><Mark />Helm</NavLink>
        <nav className="nav" aria-label="主导航">
          <NavLink to="/" end>看板</NavLink>
          <NavLink to="/machines">机器</NavLink>
        </nav>
        <div className="spacer" />
        <span className="conn" title={live ? "实时更新已连接" : "实时更新断开，正在重连"}>
          <span className={`dot ${live ? "on" : ""}`} />{live ? "实时" : "重连中"}
        </span>
      </header>
      <main>
        <Routes>
          <Route path="/" element={<Board />} />
          <Route path="/goals/:goalId" element={<Board />} />
          <Route path="/tasks/:taskId" element={<TaskPage />} />
          <Route path="/machines" element={<Machines />} />
        </Routes>
      </main>
    </div>
  );
}
