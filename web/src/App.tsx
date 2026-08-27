import { NavLink, Route, Routes } from "react-router-dom";
import Search from "./pages/Search";
import Detail from "./pages/Detail";
import Add from "./pages/Add";
import Emulator from "./pages/Emulator";

export default function App() {
  return (
    <>
      <header className="top">
        <div className="top-inner">
          <div className="brand">
            Floci<span>Flix</span>
          </div>
          <nav>
            <NavLink to="/" end>
              Search
            </NavLink>
            <NavLink to="/add">Add a movie</NavLink>
            <NavLink to="/emulator">Emulator</NavLink>
          </nav>
        </div>
      </header>
      <main className="shell">
        <Routes>
          <Route path="/" element={<Search />} />
          <Route path="/movies/:id" element={<Detail />} />
          <Route path="/add" element={<Add />} />
          <Route path="/emulator" element={<Emulator />} />
        </Routes>
      </main>
    </>
  );
}
