import { create } from 'zustand';

const useAuthStore = create((set, get) => ({
  isAuthenticated: false,
  user: null,
  token: null,

  checkAuth: () => {
    try {
      const token = localStorage.getItem('token');
      const userStr = localStorage.getItem('user');
      if (token && userStr) {
        const user = JSON.parse(userStr);
        set({ isAuthenticated: true, token, user });
        return true;
      }
    } catch (e) {
      console.error('Auth check failed:', e);
    }
    set({ isAuthenticated: false, token: null, user: null });
    return false;
  },

  login: (token, user) => {
    localStorage.setItem('token', token);
    localStorage.setItem('user', JSON.stringify(user));
    set({ isAuthenticated: true, token, user });
  },

  logout: () => {
    localStorage.removeItem('token');
    localStorage.removeItem('user');
    set({ isAuthenticated: false, token: null, user: null });
  },
}));

export default useAuthStore;
