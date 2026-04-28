import React from 'react';
import { NavLink, useNavigate } from 'react-router-dom';
import { 
  LayoutDashboard, 
  List, 
  FileText, 
  Settings, 
  LogOut,
  ChevronLeft,
  ChevronRight,
  HardDrive
} from 'lucide-react';
import useAuthStore from '../hooks/useAuthStore';

const Sidebar = ({ open, setOpen }) => {
  const navigate = useNavigate();
  const { logout } = useAuthStore();

  const handleLogout = () => {
    logout();
    navigate('/login');
  };

  const navItems = [
    { path: '/', icon: LayoutDashboard, label: '总览' },
    { path: '/tasks', icon: List, label: '任务管理' },
    { path: '/logs', icon: FileText, label: '日志查看' },
    { path: '/settings', icon: Settings, label: '系统设置' },
  ];

  return (
    <>
      {/* Mobile overlay */}
      {open && (
        <div 
          className="fixed inset-0 bg-black/50 z-40 md:hidden"
          onClick={() => setOpen(false)}
        />
      )}

      <aside className={`
        fixed top-0 left-0 z-50 h-full bg-gray-900 text-white transition-all duration-300
        ${open ? 'w-64 translate-x-0' : 'w-64 -translate-x-full md:translate-x-0 md:w-0 md:overflow-hidden'}
      `}>
        <div className="flex flex-col h-full">
          {/* Logo */}
          <div className="flex items-center justify-between px-4 py-5 border-b border-gray-800">
            <div className="flex items-center gap-3">
              <HardDrive className="w-7 h-7 text-blue-400" />
              <span className="font-bold text-lg">Rclone Manager</span>
            </div>
            <button 
              onClick={() => setOpen(false)}
              className="md:hidden p-1 hover:bg-gray-800 rounded"
            >
              <ChevronLeft className="w-5 h-5" />
            </button>
          </div>

          {/* Navigation */}
          <nav className="flex-1 px-3 py-4 space-y-1">
            {navItems.map((item) => (
              <NavLink
                key={item.path}
                to={item.path}
                onClick={() => window.innerWidth < 768 && setOpen(false)}
                className={({ isActive }) => `
                  flex items-center gap-3 px-3 py-2.5 rounded-lg transition-colors
                  ${isActive 
                    ? 'bg-blue-600 text-white' 
                    : 'text-gray-400 hover:bg-gray-800 hover:text-white'}
                `}
              >
                <item.icon className="w-5 h-5" />
                <span className="font-medium">{item.label}</span>
              </NavLink>
            ))}
          </nav>

          {/* Footer */}
          <div className="p-3 border-t border-gray-800">
            <button
              onClick={handleLogout}
              className="flex items-center gap-3 px-3 py-2.5 w-full rounded-lg text-gray-400 hover:bg-gray-800 hover:text-white transition-colors"
            >
              <LogOut className="w-5 h-5" />
              <span className="font-medium">退出登录</span>
            </button>
          </div>
        </div>
      </aside>

      {/* Toggle button for desktop */}
      <button
        onClick={() => setOpen(!open)}
        className={`
          fixed top-4 z-50 bg-gray-900 text-white p-1.5 rounded-r-lg shadow-lg
          transition-all duration-300 hidden md:block
          ${open ? 'left-64' : 'left-0'}
        `}
      >
        {open ? <ChevronLeft className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
      </button>
    </>
  );
};

export default Sidebar;
